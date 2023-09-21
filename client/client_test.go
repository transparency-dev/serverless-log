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

package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/merkle/compact"
	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/serverless-log/api"
	"golang.org/x/mod/sumdb/note"
)

var (
	testOrigin      = "Log Checkpoint v0"
	testLogVerifier = mustMakeVerifier("astra+cad5a3d2+AZJqeuyE/GnknsCNh1eCtDtwdAwKBddOlS8M2eI1Jt4b")
	// Built using serverless/testdata/build_log.sh
	testRawCheckpoints, testCheckpoints = mustLoadTestCheckpoints()
)

func b64(r string) []byte {
	ret, err := base64.StdEncoding.DecodeString(r)
	if err != nil {
		panic(err)
	}
	return ret
}

func mustMakeVerifier(vs string) note.Verifier {
	v, err := note.NewVerifier(vs)
	if err != nil {
		panic(fmt.Errorf("NewVerifier(%q): %v", vs, err))
	}
	return v
}

func mustLoadTestCheckpoints() ([][]byte, []log.Checkpoint) {
	raws, cps := make([][]byte, 0), make([]log.Checkpoint, 0)
	for i := 1; ; i++ {
		cpName := fmt.Sprintf("checkpoint.%d", i)
		r, err := testLogFetcher(context.Background(), cpName)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Probably just no more checkpoints left
				break
			}
			panic(err)
		}
		cp, _, _, err := log.ParseCheckpoint(r, testOrigin, testLogVerifier)
		if err != nil {
			panic(fmt.Errorf("ParseCheckpoint(%s): %v", cpName, err))
		}
		raws, cps = append(raws, r), append(cps, *cp)
	}
	if len(raws) == 0 {
		panic("no checkpoints loaded")
	}
	return raws, cps
}

func testLogFetcher(_ context.Context, p string) ([]byte, error) {
	path := filepath.Join("../testdata/log", p)
	return os.ReadFile(path)
}

func TestCheckLogStateTracker(t *testing.T) {
	ctx := context.Background()
	h := rfc6962.DefaultHasher

	lst := NewLogStateTracker(ctx, testLogFetcher, h, testRawCheckpoints[0])

	for _, test := range []struct {
		desc    string
		cpRaws  [][]byte
	}{} {
		{
			desc: "Consistent",
			cpRaws: [][]byte{
				testRawCheckpoints[0],
				testRawCheckpoints[2],
				testRawCheckpoints[3],
				testRawCheckpoints[5],
				testRawCheckpoints[6],
				testRawCheckpoints[10],
			},
		}, {
			desc: "Identical CP",
			cpRaws: [][]byte{
				testRawCheckpoints[0],
				testRawCheckpoints[0],
				testRawCheckpoints[0],
				testRawCheckpoints[0],
			},
		}, {
			desc: "Identical CP pairs",
			cpRaws: [][]byte{
				testRawCheckpoints[0],
				testRawCheckpoints[0],
				testRawCheckpoints[5],
				testRawCheckpoints[5],
			},
		}, {
			desc: "Out of order",
			cpRaws: [][]byte{
				testRawCheckpoints[5],
				testRawCheckpoints[2],
				testRawCheckpoints[0],
				testRawCheckpoints[3],
			},
		},
	} {
		t.Run(test.desc, func(t *testing.T){
			for i, cpRaw := range test.cpRaws {
				if err := lst.Update(ctx, cpRaw); err != nil {
					t.Errorf("Update %d: %v", i, err)
				}
			}
		})
	}
}

func TestCheckConsistency(t *testing.T) {
	ctx := context.Background()

	h := rfc6962.DefaultHasher

	for _, test := range []struct {
		desc    string
		cp      []log.Checkpoint
		wantErr bool
	}{
		{
			desc: "2 CP",
			cp: []log.Checkpoint{
				testCheckpoints[2],
				testCheckpoints[5],
			},
		}, {
			desc: "5 CP",
			cp: []log.Checkpoint{
				testCheckpoints[0],
				testCheckpoints[2],
				testCheckpoints[3],
				testCheckpoints[5],
				testCheckpoints[6],
			},
		}, {
			desc: "big CPs",
			cp: []log.Checkpoint{
				testCheckpoints[3],
				testCheckpoints[7],
				testCheckpoints[8],
			},
		}, {
			desc: "Identical CP",
			cp: []log.Checkpoint{
				testCheckpoints[0],
				testCheckpoints[0],
				testCheckpoints[0],
				testCheckpoints[0],
			},
		}, {
			desc: "Identical CP pairs",
			cp: []log.Checkpoint{
				testCheckpoints[0],
				testCheckpoints[0],
				testCheckpoints[5],
				testCheckpoints[5],
			},
		}, {
			desc: "Out of order",
			cp: []log.Checkpoint{
				testCheckpoints[5],
				testCheckpoints[2],
				testCheckpoints[0],
				testCheckpoints[3],
			},
		}, {
			desc:    "no checkpoints",
			cp:      []log.Checkpoint{},
			wantErr: true,
		}, {
			desc: "one checkpoint",
			cp: []log.Checkpoint{
				testCheckpoints[3],
			},
			wantErr: true,
		}, {
			desc: "two inconsistent CPs",
			cp: []log.Checkpoint{
				{
					Size: 2,
					Hash: []byte("This is a banana"),
				},
				testCheckpoints[4],
			},
			wantErr: true,
		}, {
			desc: "Inconsistent",
			cp: []log.Checkpoint{
				testCheckpoints[5],
				testCheckpoints[2],
				{
					Size: 4,
					Hash: []byte("This is a banana"),
				},
				testCheckpoints[3],
			},
			wantErr: true,
		}, {
			desc: "Inconsistent - clashing CPs",
			cp: []log.Checkpoint{
				{
					Size: 2,
					Hash: []byte("This is a banana"),
				},
				{
					Size: 2,
					Hash: []byte("This is NOT a banana"),
				},
			},
			wantErr: true,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			err := CheckConsistency(ctx, h, testLogFetcher, test.cp)
			if gotErr := err != nil; gotErr != test.wantErr {
				t.Fatalf("wantErr: %t, got %v", test.wantErr, err)
			}
		})
	}
}

func TestNodeCacheHandlesInvalidRequest(t *testing.T) {
	ctx := context.Background()
	wantBytes := []byte("one")
	f := func(_ context.Context, _, _ uint64) (*api.Tile, error) {
		return &api.Tile{
			Nodes: [][]byte{wantBytes},
		}, nil
	}

	// Large tree, but we're emulating skew since f, above, will return a tile which only knows about 1
	// leaf.
	nc := newNodeCache(f, 10)

	if got, err := nc.GetNode(ctx, compact.NewNodeID(0, 0)); err != nil {
		t.Errorf("got %v, want no error", err)
	} else if !bytes.Equal(got, wantBytes) {
		t.Errorf("got %v, want %v", got, wantBytes)
	}

	if _, err := nc.GetNode(ctx, compact.NewNodeID(0, 1)); err == nil {
		t.Error("got no error, want error because ID is out of range")
	}
}
