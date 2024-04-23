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
	"time"

	"github.com/transparency-dev/serverless-log/api/layout"
	"github.com/transparency-dev/serverless-log/client"
	"k8s.io/klog/v2"
)

// NewRandomLeafReader creates a RandomLeafReader.
func NewRandomLeafReader(tracker *client.LogStateTracker, f client.Fetcher, bundleSize int, throttle <-chan bool, errchan chan<- error) *RandomLeafReader {
	if bundleSize <= 0 {
		panic("bundleSize must be > 0")
	}
	return &RandomLeafReader{
		tracker:    tracker,
		f:          f,
		bundleSize: bundleSize,
		throttle:   throttle,
		errchan:    errchan,
	}
}

// RandomLeafReader reads random leaves across the tree.
type RandomLeafReader struct {
	tracker    *client.LogStateTracker
	f          client.Fetcher
	bundleSize int
	throttle   <-chan bool
	errchan    chan<- error
	cancel     func()
}

// Run runs the log reader. This should be called in a goroutine.
func (r *RandomLeafReader) Run(ctx context.Context) {
	if r.cancel != nil {
		panic("RandomLeafReader was ran multiple times")
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
		i := uint64(rand.Int63n(int64(size)))
		klog.V(2).Infof("RandomLeafReader getting %d", i)
		_, err := getLeaf(ctx, r.f, i, r.tracker.LatestConsistent.Size, r.bundleSize)
		if err != nil {
			r.errchan <- fmt.Errorf("Failed to get random leaf: %v", err)
		}
	}
}

// Kills this leaf reader at the next opportune moment.
// This function may return before the reader is dead.
func (r *RandomLeafReader) Kill() {
	if r.cancel != nil {
		r.cancel()
	}
}

// NewFullLogReader creates a FullLogReader.
func NewFullLogReader(tracker *client.LogStateTracker, f client.Fetcher, bundleSize int, throttle <-chan bool, errchan chan<- error) *FullLogReader {
	if bundleSize <= 0 {
		panic("bundleSize must be > 0")
	}
	return &FullLogReader{
		tracker:    tracker,
		f:          f,
		bundleSize: bundleSize,
		throttle:   throttle,
		errchan:    errchan,

		current: 0,
	}
}

// FullLogReader reads the whole log from the start until the end.
type FullLogReader struct {
	tracker    *client.LogStateTracker
	f          client.Fetcher
	bundleSize int
	throttle   <-chan bool
	errchan    chan<- error
	cancel     func()

	current uint64
}

// Run runs the log reader. This should be called in a goroutine.
func (r *FullLogReader) Run(ctx context.Context) {
	if r.cancel != nil {
		panic("FullLogReader was ran multiple times")
	}
	ctx, r.cancel = context.WithCancel(ctx)
	for {
		if r.current >= r.tracker.LatestConsistent.Size {
			klog.V(2).Infof("FullLogReader has consumed whole log of size %d. Sleeping.", r.tracker.LatestConsistent.Size)
			// Sleep a bit and then try again
			select {
			case <-ctx.Done(): //context cancelled
				return
			case <-time.After(2 * time.Second): //timeout
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-r.throttle:
		}
		klog.V(2).Infof("FullLogReader getting %d", r.current)
		_, err := getLeaf(ctx, r.f, r.current, r.tracker.LatestConsistent.Size, r.bundleSize)
		if err != nil {
			r.errchan <- fmt.Errorf("Failed to get next leaf: %v", err)
			continue
		}
		r.current++
	}
}

// Kills this leaf reader at the next opportune moment.
// This function may return before the reader is dead.
func (r *FullLogReader) Kill() {
	if r.cancel != nil {
		r.cancel()
	}
}

// getLeaf fetches the raw contents committed to at a given leaf index.
func getLeaf(ctx context.Context, f client.Fetcher, i uint64, logSize uint64, bundleSize int) ([]byte, error) {
	if i >= logSize {
		return nil, fmt.Errorf("requested leaf %d >= log size %d", i, logSize)
	}
	bi := i / uint64(bundleSize)
	br := uint64(0)
	// Check for partial leaf bundle
	if bi == logSize/uint64(bundleSize) {
		br = logSize % uint64(bundleSize)
	}
	p := filepath.Join(layout.SeqPath("", bi))
	if br > 0 {
		p += fmt.Sprintf(".%d", br)
	}
	bRaw, err := f(ctx, p)
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

	return base64.StdEncoding.DecodeString(string(bs[br]))
}

// NewLogWriter creates a LogWriter.
// u is the URL of the write endpoint for the log.
// gen is a function that generates new leaves to add.
func NewLogWriter(u *url.URL, gen func() []byte, throttle <-chan bool, errchan chan<- error) *LogWriter {
	return &LogWriter{
		u:        u,
		gen:      gen,
		throttle: throttle,
		errchan:  errchan,
	}
}

// LogWriter writes new leaves to the log that are generated by `gen`.
type LogWriter struct {
	u        *url.URL
	gen      func() []byte
	throttle <-chan bool
	errchan  chan<- error
	cancel   func()
}

// Run runs the log writer. This should be called in a goroutine.
func (r *LogWriter) Run(ctx context.Context) {
	if r.cancel != nil {
		panic("LogWriter was ran multiple times")
	}
	ctx, r.cancel = context.WithCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.throttle:
		}
		newLeaf := r.gen()
		resp, err := http.Post(r.u.String(), "application/octet-stream", bytes.NewReader(newLeaf))
		if err != nil {
			r.errchan <- fmt.Errorf("Failed to write leaf: %v", err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			r.errchan <- fmt.Errorf("Failed to read body: %v", err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			r.errchan <- fmt.Errorf("Write leaf was not OK. Status code: %d. Body: %q", resp.StatusCode, body)
			continue
		}
		if resp.Request.Method != http.MethodPost {
			r.errchan <- fmt.Errorf("Write leaf was redirected to %s", resp.Request.URL)
			continue
		}
		parts := bytes.Split(body, []byte("\n"))
		index, err := strconv.Atoi(string(parts[0]))
		if err != nil {
			r.errchan <- fmt.Errorf("Write leaf failed to parse response: %v", body)
			continue
		}

		klog.V(2).Infof("Wrote leaf at index %d", index)
	}
}

// Kills this writer at the next opportune moment.
// This function may return before the writer is dead.
func (r *LogWriter) Kill() {
	if r.cancel != nil {
		r.cancel()
	}
}
