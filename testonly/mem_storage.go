// Copyright 2023 Google LLC. All Rights Reserved.
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

// package testonly provides helpers which are intended for use in tests.
package testonly

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"k8s.io/klog/v2"

	"github.com/transparency-dev/serverless-log/api"
	"github.com/transparency-dev/serverless-log/api/layout"
	"github.com/transparency-dev/serverless-log/client"
	"github.com/transparency-dev/serverless-log/pkg/log"
)

type MemStorage struct {
	sync.Mutex
	fs      map[string][]byte
	nextSeq uint64
}

var _ log.Storage = &MemStorage{}

func NewMemStorage() *MemStorage {
	return &MemStorage{
		fs: make(map[string][]byte),
	}
}

// GetTile returns the tile at the given level & index.
func (ms *MemStorage) GetTile(_ context.Context, level, index, logSize uint64) (*api.Tile, error) {
	ms.Lock()
	defer ms.Unlock()
	tileSize := layout.PartialTileSize(level, index, logSize)
	d, k := layout.TilePath("", level, index, tileSize)
	t, ok := ms.fs[filepath.Join(d, k)]
	if !ok {
		return nil, os.ErrNotExist
	}
	tile := &api.Tile{}
	if err := tile.UnmarshalText(t); err != nil {
		return nil, err
	}
	return tile, nil
}

// StoreTile stores the tile at the given level & index.
func (ms *MemStorage) StoreTile(_ context.Context, level, index uint64, tile *api.Tile) error {
	ms.Lock()
	defer ms.Unlock()

	t, err := tile.MarshalText()
	if err != nil {
		return err
	}

	tileSize := uint64(tile.NumLeaves)
	d, k := layout.TilePath("", level, index, tileSize%256)
	klog.Infof("Store tile %s", filepath.Join(d, k))
	ms.fs[filepath.Join(d, k)] = t
	return nil
}

// WriteCheckpoint stores a newly updated log checkpoint.
func (ms *MemStorage) WriteCheckpoint(_ context.Context, newCPRaw []byte) error {
	ms.Lock()
	defer ms.Unlock()
	k := layout.CheckpointPath
	ms.fs[k] = newCPRaw
	return nil
}

// Sequence assigns sequence numbers to the passed in entry.
// Returns the assigned sequence number for the leafhash.
//
// If a duplicate leaf is sequenced the storage implementation may return
// the sequence number associated with an earlier instance, along with a
// ErrDupeLeaf error.
func (ms *MemStorage) Sequence(_ context.Context, leafhash []byte, leaf []byte) (uint64, error) {
	ms.Lock()
	defer ms.Unlock()

	seq := ms.nextSeq
	ms.nextSeq++

	ds, ks := layout.SeqPath("", seq)
	ms.fs[filepath.Join(ds, ks)] = leaf
	dl, kl := layout.LeafPath("", leafhash)
	ms.fs[filepath.Join(dl, kl)] = []byte(strconv.FormatUint(seq, 16))
	return seq, nil

}

// ScanSequenced calls f for each contiguous sequenced log entry >= begin.
// It should stop scanning if the call to f returns an error.
func (ms *MemStorage) ScanSequenced(_ context.Context, begin uint64, f func(seq uint64, entry []byte) error) (uint64, error) {
	// No lock since we're only looking at immutable data
	for i := begin; ; i++ {
		ds, ks := layout.SeqPath("", i)
		e, ok := ms.fs[filepath.Join(ds, ks)]
		if !ok {
			return i, nil
		}
		if err := f(i, e); err != nil {
			return i, err
		}
	}
}

func (ms *MemStorage) Fetcher() client.Fetcher {
	return func(_ context.Context, path string) ([]byte, error) {
		ms.Lock()
		defer ms.Unlock()
		klog.Infof("Fetch %s", path)
		r, ok := ms.fs[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return r, nil
	}
}
