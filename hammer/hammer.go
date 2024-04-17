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

	// Kick off readers
	errChan := make(chan error, 20)
	throttle := make(chan bool, *maxReadOpsPerSecond)
	for i := 0; i < *numReadersRandom; i++ {
		go NewRandomLeafReader(&tracker, f, throttle, errChan).Run(ctx)
	}
	for i := 0; i < *numReadersFull; i++ {
		go NewFullLogReader(&tracker, f, throttle, errChan).Run(ctx)
	}

	// Set up log throttle token generator
	go func() {
		for {
			select {
			case <-ctx.Done(): //context cancelled
				return
			case <-time.After(1 * time.Second): //timeout
			}
			for i := 0; i < *maxReadOpsPerSecond; i++ {
				throttle <- true
			}
		}
	}()

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

	// TODO(mhutchinson): Set up writing

	klog.Info("It's hammer time")
	<-ctx.Done()
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
