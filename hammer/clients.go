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
	"context"
	"fmt"
	"math/rand"
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
		i := uint64(rand.Int63n(int64(r.tracker.LatestConsistent.Size)))
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
		klog.V(2).Infof("FullLeafReader getting %d", r.current)
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
