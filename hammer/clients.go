// Copyright 2024 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/transparency-dev/serverless-log/api/layout"
	"github.com/transparency-dev/serverless-log/client"
	"k8s.io/klog/v2"
)

// NewLeafReader creates a LeafReader.
// The next function provides a strategy for which leaves will be read.
// Custom implementations can be passed, or use RandomNextLeaf or MonotonicallyIncreasingNextLeaf.
func NewLeafReader(tracker *client.LogStateTracker, f client.Fetcher, next func(uint64) uint64, bundleSize int, throttle <-chan bool, errchan chan<- error) *LeafReader {
	if bundleSize <= 0 {
		panic("bundleSize must be > 0")
	}
	return &LeafReader{
		tracker:    tracker,
		f:          f,
		next:       next,
		bundleSize: bundleSize,
		throttle:   throttle,
		errchan:    errchan,
	}
}

// LeafReader reads leaves from the tree.
type LeafReader struct {
	tracker    *client.LogStateTracker
	f          client.Fetcher
	next       func(uint64) uint64
	bundleSize int
	throttle   <-chan bool
	errchan    chan<- error
	cancel     func()
	c          tileCache
}

// Run runs the log reader. This should be called in a goroutine.
func (r *LeafReader) Run(ctx context.Context) {
	if r.cancel != nil {
		panic("LeafReader was ran multiple times")
	}
	ctx, r.cancel = context.WithCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.throttle:
		}
		size := r.tracker.LatestConsistent.Size
		if size == 0 {
			continue
		}
		i := r.next(size)
		if i >= size {
			continue
		}
		klog.V(2).Infof("LeafReader getting %d", i)
		_, err := r.getLeaf(ctx, i, size)
		if err != nil {
			r.errchan <- fmt.Errorf("failed to get leaf: %v", err)
		}
	}
}

// getLeaf fetches the raw contents committed to at a given leaf index.
func (r *LeafReader) getLeaf(ctx context.Context, i uint64, logSize uint64) ([]byte, error) {
	if i >= logSize {
		return nil, fmt.Errorf("requested leaf %d >= log size %d", i, logSize)
	}
	if cached := r.c.get(i); cached != nil {
		klog.V(2).Infof("Using cached result for index %d", i)
		return cached, nil
	}
	bi := i / uint64(r.bundleSize)
	br := uint64(0)
	// Check for partial leaf bundle
	if bi == logSize/uint64(r.bundleSize) {
		br = logSize % uint64(r.bundleSize)
	}
	p := filepath.Join(layout.SeqPath("", bi))
	if br > 0 {
		p += fmt.Sprintf(".%d", br)
	}
	bRaw, err := r.f(ctx, p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("leaf index %d not found: %w", i, err)
		}
		return nil, fmt.Errorf("failed to fetch leaf index %d: %w", i, err)
	}
	bs := bytes.Split(bRaw, []byte("\n"))
	if l := len(bs); uint64(l) <= br {
		return nil, fmt.Errorf("huh, short leaf bundle with %d entries, want %d", l, br)
	}
	r.c = tileCache{
		start:  bi * uint64(r.bundleSize),
		leaves: bs,
	}

	return base64.StdEncoding.DecodeString(string(bs[br]))
}

// Kills this leaf reader at the next opportune moment.
// This function may return before the reader is dead.
func (r *LeafReader) Kill() {
	if r.cancel != nil {
		r.cancel()
	}
}

// tileCache stores the results of the last fetched tile. This allows
// readers that read contiguous blocks of leaves to act more like real
// clients and fetch a tile of 256 leaves once, instead of 256 times.
type tileCache struct {
	start  uint64
	leaves [][]byte
}

func (tc tileCache) get(i uint64) []byte {
	end := tc.start + uint64(len(tc.leaves))
	if i >= tc.start && i < end {
		return tc.leaves[i-tc.start]
	}
	return nil
}

// RandomNextLeaf returns a function that fetches a random leaf available in the tree.
func RandomNextLeaf() func(uint64) uint64 {
	return func(size uint64) uint64 {
		return uint64(rand.Int63n(int64(size)))
	}
}

// MonotonicallyIncreasingNextLeaf returns a function that always wants the next available
// leaf after the one it previously fetched. It starts at leaf 0.
func MonotonicallyIncreasingNextLeaf() func(uint64) uint64 {
	var i uint64
	return func(size uint64) uint64 {
		if i < size {
			r := i
			i++
			return r
		}
		return size
	}
}

// NewLogWriter creates a LogWriter.
// u is the URL of the write endpoint for the log.
// gen is a function that generates new leaves to add.
func NewLogWriter(hc *http.Client, u *url.URL, gen func() []byte, throttle <-chan bool, errchan chan<- error) *LogWriter {
	return &LogWriter{
		hc:       hc,
		u:        u,
		gen:      gen,
		throttle: throttle,
		errchan:  errchan,
	}
}

// LogWriter writes new leaves to the log that are generated by `gen`.
type LogWriter struct {
	hc       *http.Client
	u        *url.URL
	gen      func() []byte
	throttle <-chan bool
	errchan  chan<- error
	cancel   func()
}

// Run runs the log writer. This should be called in a goroutine.
func (w *LogWriter) Run(ctx context.Context) {
	if w.cancel != nil {
		panic("LogWriter was ran multiple times")
	}
	ctx, w.cancel = context.WithCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.throttle:
		}
		newLeaf := w.gen()

		resp, err := w.hc.Post(w.u.String(), "application/octet-stream", bytes.NewReader(newLeaf))
		if err != nil {
			w.errchan <- fmt.Errorf("failed to write leaf: %v", err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			w.errchan <- fmt.Errorf("failed to read body: %v", err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			w.errchan <- fmt.Errorf("write leaf was not OK. Status code: %d. Body: %q", resp.StatusCode, body)
			continue
		}
		if resp.Request.Method != http.MethodPost {
			w.errchan <- fmt.Errorf("write leaf was redirected to %s", resp.Request.URL)
			continue
		}
		parts := bytes.Split(body, []byte("\n"))
		index, err := strconv.Atoi(string(parts[0]))
		if err != nil {
			w.errchan <- fmt.Errorf("write leaf failed to parse response: %v", body)
			continue
		}

		klog.V(2).Infof("Wrote leaf at index %d", index)
	}
}

// Kills this writer at the next opportune moment.
// This function may return before the writer is dead.
func (w *LogWriter) Kill() {
	if w.cancel != nil {
		w.cancel()
	}
}
