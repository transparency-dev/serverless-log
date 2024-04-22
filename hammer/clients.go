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
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/transparency-dev/serverless-log/client"
	"k8s.io/klog/v2"
)

// NewRandomLeafReader creates a RandomLeafReader.
func NewRandomLeafReader(tracker *client.LogStateTracker, f client.Fetcher, throttle <-chan bool, errchan chan<- error) *RandomLeafReader {
	return &RandomLeafReader{
		tracker:  tracker,
		f:        f,
		throttle: throttle,
		errchan:  errchan,
	}
}

// RandomLeafReader reads random leaves across the tree.
type RandomLeafReader struct {
	tracker  *client.LogStateTracker
	f        client.Fetcher
	throttle <-chan bool
	errchan  chan<- error
	cancel   func()
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
		_, err := client.GetLeaf(ctx, r.f, i)
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
func NewFullLogReader(tracker *client.LogStateTracker, f client.Fetcher, throttle <-chan bool, errchan chan<- error) *FullLogReader {
	return &FullLogReader{
		tracker:  tracker,
		f:        f,
		throttle: throttle,
		errchan:  errchan,

		current: 0,
	}
}

// FullLogReader reads the whole log from the start until the end.
type FullLogReader struct {
	tracker  *client.LogStateTracker
	f        client.Fetcher
	throttle <-chan bool
	errchan  chan<- error
	cancel   func()

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
		_, err := client.GetLeaf(ctx, r.f, r.current)
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

// LogWriter reads the whole log from the start until the end.
type LogWriter struct {
	u        *url.URL
	gen      func() []byte
	throttle <-chan bool
	errchan  chan<- error
	cancel   func()
}

// Run runs the log reader. This should be called in a goroutine.
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

// Kills this leaf reader at the next opportune moment.
// This function may return before the reader is dead.
func (r *LogWriter) Kill() {
	if r.cancel != nil {
		r.cancel()
	}
}
