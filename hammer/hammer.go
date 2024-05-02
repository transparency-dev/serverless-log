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

// hammer is a tool to load test a serverless log.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/serverless-log/client"
	"golang.org/x/mod/sumdb/note"
	"k8s.io/klog/v2"
)

var (
	logURL        = flag.String("log_url", "", "Log storage root URL, e.g. https://log.server/and/path/")
	logPubKeyFile = flag.String("log_public_key", "", "Location of log public key file. If unset, uses the contents of the SERVERLESS_LOG_PUBLIC_KEY environment variable")
	origin        = flag.String("origin", "", "Expected first line of checkpoints from log")

	maxReadOpsPerSecond = flag.Int("max_read_ops", 20, "The maximum number of read operations per second")
	numReadersRandom    = flag.Int("num_readers_random", 4, "The number of readers looking for random leaves")
	numReadersFull      = flag.Int("num_readers_full", 4, "The number of readers downloading the whole log")

	maxWriteOpsPerSecond = flag.Int("max_write_ops", 0, "The maximum number of write operations per second")
	numWriters           = flag.Int("num_writers", 0, "The number of independent write tasks to run")

	leafBundleSize = flag.Int("leaf_bundle_size", 1, "The log-configured number of leaves in each leaf bundle")

	showUI = flag.Bool("show_ui", true, "Set to false to disable the text-based UI")

	hc = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			DisableKeepAlives:   false,
		},
	}
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	ctx := context.Background()

	logSigV, _, err := logSigVerifier(*logPubKeyFile)
	if err != nil {
		klog.Exitf("failed to read log public key: %v", err)
	}

	u := *logURL
	if len(u) == 0 {
		klog.Exitf("--log_url must be provided")
	}
	// url must reference a directory, by definition
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}

	rootURL, err := url.Parse(u)
	if err != nil {
		klog.Exitf("Invalid log URL: %v", err)
	}

	var cpRaw []byte
	f := newFetcher(rootURL)
	cons := client.UnilateralConsensus(f)
	hasher := rfc6962.DefaultHasher
	tracker, err := client.NewLogStateTracker(ctx, f, hasher, cpRaw, logSigV, *origin, cons)
	if err != nil {
		klog.Exitf("Failed to create LogStateTracker: %v", err)
	}
	// Fetch initial state of log
	_, _, _, err = tracker.Update(ctx)
	if err != nil {
		klog.Exitf("Failed to get initial state of the log: %v", err)
	}

	addURL, err := rootURL.Parse("add")
	if err != nil {
		klog.Exitf("Failed to create add URL: %v", err)
	}
	hammer := NewHammer(&tracker, f, addURL)
	hammer.Run(ctx)

	if *showUI {
		hostUI(ctx, hammer)
	} else {
		<-ctx.Done()
	}
}

func NewHammer(tracker *client.LogStateTracker, f client.Fetcher, addURL *url.URL) *Hammer {
	readThrottle := NewThrottle(*maxReadOpsPerSecond)
	writeThrottle := NewThrottle(*maxWriteOpsPerSecond)
	errChan := make(chan error, 20)

	randomReaders := make([]*LeafReader, *numReadersRandom)
	fullReaders := make([]*LeafReader, *numReadersFull)
	writers := make([]*LogWriter, *numWriters)
	for i := 0; i < *numReadersRandom; i++ {
		randomReaders[i] = NewLeafReader(tracker, f, RandomNextLeaf(), *leafBundleSize, readThrottle.tokenChan, errChan)
	}
	for i := 0; i < *numReadersFull; i++ {
		fullReaders[i] = NewLeafReader(tracker, f, MonotonicallyIncreasingNextLeaf(), *leafBundleSize, readThrottle.tokenChan, errChan)
	}
	gen := newLeafGenerator()
	for i := 0; i < *numWriters; i++ {
		writers[i] = NewLogWriter(hc, addURL, gen, writeThrottle.tokenChan, errChan)
	}
	return &Hammer{
		randomReaders: randomReaders,
		fullReaders:   fullReaders,
		writers:       writers,
		readThrottle:  readThrottle,
		writeThrottle: writeThrottle,
		tracker:       tracker,
		errChan:       errChan,
	}
}

type Hammer struct {
	randomReaders []*LeafReader
	fullReaders   []*LeafReader
	writers       []*LogWriter
	readThrottle  *Throttle
	writeThrottle *Throttle
	tracker       *client.LogStateTracker
	errChan       chan error
}

func (h *Hammer) Run(ctx context.Context) {
	// Kick off readers & writers
	for _, r := range h.randomReaders {
		go r.Run(ctx)
	}
	for _, r := range h.fullReaders {
		go r.Run(ctx)
	}
	for _, w := range h.writers {
		go w.Run(ctx)
	}

	// Set up logging for any errors
	go func() {
		for {
			select {
			case <-ctx.Done(): //context cancelled
				return
			case err := <-h.errChan:
				klog.Warning(err)
			}
		}
	}()

	// Start the throttles
	go h.readThrottle.Run(ctx)
	go h.writeThrottle.Run(ctx)

	go func() {
		tick := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				size := h.tracker.LatestConsistent.Size
				_, _, _, err := h.tracker.Update(ctx)
				if err != nil {
					klog.Warning(err)
				}
				newSize := h.tracker.LatestConsistent.Size
				if newSize > size {
					klog.V(1).Infof("Updated checkpoint from %d to %d", size, newSize)
				}
			}
		}
	}()
}

func newLeafGenerator() func() []byte {
	const dupChance = 0.1
	var g int64
	return func() []byte {
		var r int64
		if rand.Float64() <= dupChance {
			// This one will actually be unique, but the next iteration will
			// duplicate it. In future, this duplication could be randomly
			// selected to include really old leaves too, to test long-term
			// deduplication in the log (if it supports  that).
			r = g
		} else {
			r = g
			g++
		}
		return []byte(fmt.Sprintf("%d", r))
	}
}

func NewThrottle(opsPerSecond int) *Throttle {
	return &Throttle{
		opsPerSecond: opsPerSecond,
		tokenChan:    make(chan bool, opsPerSecond),
	}
}

type Throttle struct {
	opsPerSecond int
	tokenChan    chan bool

	oversupply int
}

func (t *Throttle) Increase() {
	tokenCount := t.opsPerSecond
	delta := float64(tokenCount) * 0.1
	if delta < 1 {
		delta = 1
	}
	t.opsPerSecond = tokenCount + int(delta)
}

func (t *Throttle) Decrease() {
	tokenCount := t.opsPerSecond
	if tokenCount <= 1 {
		return
	}
	delta := float64(tokenCount) * 0.1
	if delta < 1 {
		delta = 1
	}
	t.opsPerSecond = tokenCount - int(delta)
}

func (t *Throttle) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ctx.Done(): //context cancelled
			return
		case <-ticker.C:
			tokenCount := t.opsPerSecond
			timeout := time.After(1 * time.Second)
		Loop:
			for i := 0; i < t.opsPerSecond; i++ {
				select {
				case t.tokenChan <- true:
					tokenCount--
				case <-timeout:
					break Loop
				}
			}
			t.oversupply = tokenCount
		}
	}
}

func (t *Throttle) String() string {
	return fmt.Sprintf("Current max: %d/s. Oversupply in last second: %d", t.opsPerSecond, t.oversupply)
}

func hostUI(ctx context.Context, hammer *Hammer) {
	grid := tview.NewGrid()
	grid.SetRows(3, 0, 10).SetColumns(0).SetBorders(true)
	// Status box
	statusView := tview.NewTextView()
	grid.AddItem(statusView, 0, 0, 1, 1, 0, 0, false)
	// Log view box
	logView := tview.NewTextView()
	logView.ScrollToEnd()
	grid.AddItem(logView, 1, 0, 1, 1, 0, 0, false)
	if err := flag.Set("logtostderr", "false"); err != nil {
		klog.Exitf("Failed to set flag: %v", err)
	}
	if err := flag.Set("alsologtostderr", "false"); err != nil {
		klog.Exitf("Failed to set flag: %v", err)
	}
	klog.SetOutput(logView)

	helpView := tview.NewTextView()
	helpView.SetText("+/- to increase/decrease read load\n>/< to increase/decrease write load")
	grid.AddItem(helpView, 2, 0, 1, 1, 0, 0, false)

	app := tview.NewApplication()
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				text := fmt.Sprintf("Read: %s\nWrite: %s", hammer.readThrottle.String(), hammer.writeThrottle.String())
				statusView.SetText(text)
				app.Draw()
			}
		}
	}()
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case '+':
			klog.Info("Increasing the read operations per second")
			hammer.readThrottle.Increase()
		case '-':
			klog.Info("Decreasing the read operations per second")
			hammer.readThrottle.Decrease()
		case '>':
			klog.Info("Increasing the write operations per second")
			hammer.writeThrottle.Increase()
		case '<':
			klog.Info("Decreasing the write operations per second")
			hammer.writeThrottle.Decrease()
		}
		return event
	})
	// logView.SetChangedFunc(func() {
	// 	app.Draw()
	// })
	if err := app.SetRoot(grid, true).Run(); err != nil {
		panic(err)
	}
}

// Returns a log signature verifier and the public key bytes it uses.
// Attempts to read key material from f, or uses the SERVERLESS_LOG_PUBLIC_KEY
// env var if f is unset.
func logSigVerifier(f string) (note.Verifier, []byte, error) {
	var pubKey []byte
	var err error
	if len(f) > 0 {
		pubKey, err = os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read public key from file %q: %v", f, err)
		}
	} else {
		pubKey = []byte(os.Getenv("SERVERLESS_LOG_PUBLIC_KEY"))
		if len(pubKey) == 0 {
			return nil, nil, fmt.Errorf("supply public key file path using --log_public_key or set SERVERLESS_LOG_PUBLIC_KEY environment variable")
		}
	}

	v, err := note.NewVerifier(string(pubKey))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create verifier: %v", err)
	}

	return v, pubKey, nil
}

// newFetcher creates a Fetcher for the log at the given root location.
func newFetcher(root *url.URL) client.Fetcher {
	get := getByScheme[root.Scheme]
	if get == nil {
		panic(fmt.Errorf("unsupported URL scheme %s", root.Scheme))
	}

	return func(ctx context.Context, p string) ([]byte, error) {
		u, err := root.Parse(p)
		if err != nil {
			return nil, err
		}
		return get(ctx, u)
	}
}

var getByScheme = map[string]func(context.Context, *url.URL) ([]byte, error){
	"http":  readHTTP,
	"https": readHTTP,
	"file": func(_ context.Context, u *url.URL) ([]byte, error) {
		return os.ReadFile(u.Path)
	},
}

func readHTTP(ctx context.Context, u *url.URL) ([]byte, error) {
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case 404:
		klog.Infof("Not found: %q", u.String())
		return nil, os.ErrNotExist
	case 200:
		break
	default:
		return nil, fmt.Errorf("unexpected http status %q", resp.Status)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.Errorf("resp.Body.Close(): %v", err)
		}
	}()
	return io.ReadAll(resp.Body)
}
