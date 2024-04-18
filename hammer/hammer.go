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

	showUI = flag.Bool("show_ui", true, "Set to false to disable the text-based UI")
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

	hammer := NewHammer()
	go hammer.Run(ctx, &tracker, f)

	// TODO(mhutchinson): Set up writing

	if *showUI {
		hostUI(ctx, hammer)
	} else {
		<-ctx.Done()
	}
}

func NewHammer() *Hammer {
	return &Hammer{
		randomReaders: make([]*RandomLeafReader, *numReadersRandom),
		fullReaders:   make([]*FullLogReader, *numReadersFull),
		throttle:      NewThrottle(),
	}
}

type Hammer struct {
	randomReaders []*RandomLeafReader
	fullReaders   []*FullLogReader
	throttle      *Throttle
}

func (h *Hammer) Run(ctx context.Context, tracker *client.LogStateTracker, f client.Fetcher) {
	// Kick off readers
	errChan := make(chan error, 20)
	for i := 0; i < *numReadersRandom; i++ {
		h.randomReaders[i] = NewRandomLeafReader(tracker, f, h.throttle.readTokens, errChan)
		go h.randomReaders[i].Run(ctx)
	}
	for i := 0; i < *numReadersFull; i++ {
		h.fullReaders[i] = NewFullLogReader(tracker, f, h.throttle.readTokens, errChan)
		go h.fullReaders[i].Run(ctx)
	}

	// Set up logging for any errors
	go func() {
		for {
			select {
			case <-ctx.Done(): //context cancelled
				return
			case err := <-errChan:
				klog.Warning(err)
			}
		}
	}()

	// Start the throttle
	go h.throttle.Run(ctx)
}

func NewThrottle() *Throttle {
	return &Throttle{
		readOpsPerSecond: *maxReadOpsPerSecond,
		readTokens:       make(chan bool, *maxReadOpsPerSecond),
	}
}

type Throttle struct {
	readOpsPerSecond int
	readTokens       chan bool

	oversupply int
}

func (t *Throttle) Increase() {
	tokenCount := t.readOpsPerSecond
	delta := float64(tokenCount) * 0.1
	if delta < 1 {
		delta = 1
	}
	t.readOpsPerSecond = tokenCount + int(delta)
}

func (t *Throttle) Decrease() {
	tokenCount := t.readOpsPerSecond
	if tokenCount <= 1 {
		return
	}
	delta := float64(tokenCount) * 0.1
	if delta < 1 {
		delta = 1
	}
	t.readOpsPerSecond = tokenCount - int(delta)
}

func (t *Throttle) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ctx.Done(): //context cancelled
			return
		case <-ticker.C:
			tokenCount := t.readOpsPerSecond
			sessionOversupply := 0
			for i := 0; i < tokenCount; i++ {
				select {
				case t.readTokens <- true:
				default:
					sessionOversupply += 1
				}
			}
			t.oversupply = sessionOversupply
		}
	}
}

func (t *Throttle) String() string {
	return fmt.Sprintf("Current max: %d reads/s. Oversupply in last second: %d", t.readOpsPerSecond, t.oversupply)
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

	app := tview.NewApplication()
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				statusView.SetText(hammer.throttle.String())
				app.Draw()
			}
		}
	}()
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case '+':
			klog.Info("Increasing the read operations per second")
			hammer.throttle.Increase()
		case '-':
			klog.Info("Decreasing the read operations per second")
			hammer.throttle.Decrease()
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
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
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
