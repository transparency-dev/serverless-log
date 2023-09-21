// Copyright 2021 Google LLC. All Rights Reserved.
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

// Package integration provides an integration test for the serverless example.
package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/serverless-log/client"
	"github.com/transparency-dev/serverless-log/internal/storage/fs"
	"golang.org/x/mod/sumdb/note"
)

func TestServerlessViaFile(t *testing.T) {
	t.Parallel()

	h := rfc6962.DefaultHasher

	// Create log instance
	root := filepath.Join(t.TempDir(), "log")

	// Create signer
	s := mustGetSigner(t, privKey)

	// Create empty checkpoint
	st := mustCreateAndInitialiseStorage(context.Background(), t, root, s)

	// Create file fetcher
	rootURL, err := url.Parse(fmt.Sprintf("file://%s/", root))
	if err != nil {
		t.Fatalf("Failed to create root URL: %q", err)
	}
	f := func(_ context.Context, p string) ([]byte, error) {
		u, err := rootURL.Parse(p)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(u.Path)
	}

	// Run test
	RunIntegration(t, st, f, h)
}

func TestServerlessViaHTTP(t *testing.T) {
	t.Parallel()

	h := rfc6962.DefaultHasher

	// Create log instance
	root := filepath.Join(t.TempDir(), "log")

	// Create signer
	s := mustGetSigner(t, privKey)

	// Create empty checkpoint
	st := mustCreateAndInitialiseStorage(context.Background(), t, root, s)

	// Arrange for its files to be served via HTTP
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Failed to create listener: %q", err)
	}
	srv := http.Server{
		Handler: http.FileServer(http.Dir(root)),
	}
	defer func() {
		if err := srv.Close(); err != nil {
			t.Errorf("srv.Close(): %v", err)
		}
	}()
	go func() {
		if err := srv.Serve(listener); err != http.ErrServerClosed {
			t.Error(err)
		}
	}()

	// Create fetcher
	url := fmt.Sprintf("http://%s/", listener.Addr().String())
	f := httpFetcher(t, url)

	// Run test
	RunIntegration(t, st, f, h)
}

func httpFetcher(t *testing.T, u string) client.Fetcher {
	t.Helper()
	rootURL, err := url.Parse(u)
	if err != nil {
		t.Fatalf("Failed to create root URL: %q", err)
	}

	return func(ctx context.Context, p string) ([]byte, error) {
		u, err := rootURL.Parse(p)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Errorf("resp.Body.Close(): %v", err)
			}
		}()
		return io.ReadAll(resp.Body)
	}
}

func mustCreateAndInitialiseStorage(ctx context.Context, t *testing.T, root string, s note.Signer) *fs.Storage {
	t.Helper()
	st, err := fs.Create(root)
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if err := InitialiseStorage(ctx, t, st); err != nil {
		t.Fatalf("InitialiseStorage = %v", err)
	}
	return st
}
