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

// Package storage provides a log storage implementation on Google Cloud Storage (GCS).
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/transparency-dev/serverless-log/api"
	"github.com/transparency-dev/serverless-log/api/layout"
	"github.com/transparency-dev/serverless-log/pkg/log"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"k8s.io/klog/v2"

	gcs "cloud.google.com/go/storage"
)

// Client is a serverless storage implementation which uses a GCS bucket to store tree state.
// The naming of the objects of the GCS object is:
//
//	leaves/aa/bb/cc/ddeeff...
//	seq/aa/bb/cc/ddeeff...
//	tile/<level>/aa/bb/ccddee...
//	checkpoint
//
// The functions on this struct are not thread-safe.
type Client struct {
	gcsClient *gcs.Client
	projectID string
	// bucket is the name of the bucket where tree data will be stored.
	bucket string
	// nextSeq is a hint to the Sequence func as to what the next available
	// sequence number is to help performance.
	// Note that nextSeq may be <= than the actual next available number, but
	// never greater.
	nextSeq uint64
	// checkpointGen is the GCS object generation number that this client last
	// read. This is useful for read-modify-write operation of the checkpoint.
	checkpointGen int64

	checkpointCacheControl string
	otherCacheControl      string
}

// ClientOpts holds configuration options for the storage client.
type ClientOpts struct {
	// ProjectID is the GCP project which hosts the storage bucket for the log.
	ProjectID string
	// Bucket is the name of the bucket to use for storing log state.
	Bucket string
	// CheckpointCacheControl, if set, will cause the Cache-Control header associated with the
	// checkpoint object to be set to this value. If unset, the current GCP default will be used.
	CheckpointCacheControl string
	// OtherCacheControl, if set, will cause the Cache-Control header associated with the
	// all non-checkpoint objects to be set to this value. If unset, the current GCP default
	// will be used.
	OtherCacheControl string
}

// NewClient returns a Client which allows interaction with the log stored in
// the specified bucket on GCS.
func NewClient(ctx context.Context, opts ClientOpts) (*Client, error) {
	c, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	return &Client{
		gcsClient:              c,
		projectID:              opts.ProjectID,
		bucket:                 opts.Bucket,
		checkpointGen:          0,
		checkpointCacheControl: opts.CheckpointCacheControl,
		otherCacheControl:      opts.OtherCacheControl,
	}, nil
}

func (c *Client) bucketExists(ctx context.Context, bucket string) (bool, error) {
	it := c.gcsClient.Buckets(ctx, c.projectID)
	for {
		bAttrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return false, err
		}
		if bAttrs.Name == bucket {
			return true, nil
		}
	}
	return false, nil
}

// Create creates a new GCS bucket and returns an error on failure.
func (c *Client) Create(ctx context.Context, bucket string) error {
	// Check if the bucket already exists.
	exists, err := c.bucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("expected bucket %q to not be created yet)", bucket)
	}

	// Create the bucket.
	bkt := c.gcsClient.Bucket(bucket)
	if err := bkt.Create(ctx, c.projectID, nil); err != nil {
		return fmt.Errorf("failed to create bucket %q in project %s: %w", bucket, c.projectID, err)
	}
	bkt.ACL().Set(ctx, gcs.AllUsers, gcs.RoleReader)

	c.bucket = bucket
	c.nextSeq = 0
	return nil
}

// SetNextSeq sets the input as the nextSeq of the client.
func (c *Client) SetNextSeq(num uint64) {
	c.nextSeq = num
}

// WriteCheckpoint stores a raw log checkpoint on GCS if it matches the
// generation that the client thinks the checkpoint is. The client updates the
// generation number of the checkpoint whenever ReadCheckpoint is called.
//
// This method will fail to write if 1) the checkpoint exists and the client
// has never read it or 2) the checkpoint has been updated since the client
// called ReadCheckpoint.
func (c *Client) WriteCheckpoint(ctx context.Context, newCPRaw []byte) error {
	bkt := c.gcsClient.Bucket(c.bucket)
	obj := bkt.Object(layout.CheckpointPath)

	var cond gcs.Conditions
	if c.checkpointGen == 0 {
		cond = gcs.Conditions{DoesNotExist: true}
	} else {
		cond = gcs.Conditions{GenerationMatch: c.checkpointGen}
	}

	w := obj.If(cond).NewWriter(ctx)
	if c.checkpointCacheControl != "" {
		w.ObjectAttrs.CacheControl = c.checkpointCacheControl
	}
	if _, err := w.Write(newCPRaw); err != nil {
		return err
	}
	return w.Close()
}

// ReadCheckpoint reads from GCS and returns the contents of the log checkpoint.
func (c *Client) ReadCheckpoint(ctx context.Context) ([]byte, error) {
	bkt := c.gcsClient.Bucket(c.bucket)
	obj := bkt.Object(layout.CheckpointPath)

	// Get the GCS generation number.
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, fmt.Errorf("Object(%q).Attrs: %w", obj, err)
	}
	c.checkpointGen = attrs.Generation

	// Get the content of the checkpoint.
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return io.ReadAll(r)
}

// GetTile returns the tile at the given tile-level and tile-index.
// If no complete tile exists at that location, it will attempt to find a
// partial tile for the given tree size at that location.
func (c *Client) GetTile(ctx context.Context, level, index, logSize uint64) (*api.Tile, error) {
	tileSize := layout.PartialTileSize(level, index, logSize)
	bkt := c.gcsClient.Bucket(c.bucket)

	// Pass an empty rootDir since we don't need this concept in GCS.
	objName := filepath.Join(layout.TilePath("", level, index, tileSize))
	r, err := bkt.Object(objName).NewReader(ctx)
	if err != nil {
		fmt.Printf("GetTile: failed to create reader for object %q in bucket %q: %v", objName, c.bucket, err)

		if errors.Is(err, gcs.ErrObjectNotExist) {
			// Return the generic NotExist error so that tileCache.Visit can differentiate
			// between this and other errors.
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	defer r.Close()

	t, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read tile object %q in bucket %q: %v", objName, c.bucket, err)
	}

	var tile api.Tile
	if err := tile.UnmarshalText(t); err != nil {
		return nil, fmt.Errorf("failed to parse tile: %w", err)
	}
	return &tile, nil
}

// ScanSequenced calls the provided function once for each contiguous entry
// in storage starting at begin.
// The scan will abort if the function returns an error, otherwise it will
// return the number of sequenced entries scanned.
func (c *Client) ScanSequenced(ctx context.Context, begin uint64, f func(seq uint64, entry []byte) error) (uint64, error) {
	end := begin
	bkt := c.gcsClient.Bucket(c.bucket)

	for {
		// Pass an empty rootDir since we don't need this concept in GCS.
		sp := filepath.Join(layout.SeqPath("", end))

		// Read the object in an anonymous function so that the reader gets closed
		// in each iteration of the outside for loop.
		done, err := func() (bool, error) {
			r, err := bkt.Object(sp).NewReader(ctx)
			if errors.Is(err, gcs.ErrObjectNotExist) {
				// we're done.
				return true, nil
			} else if err != nil {
				return false, fmt.Errorf("ScanSequenced: failed to create reader for object %q in bucket %q: %v", sp, c.bucket, err)
			}
			defer r.Close()

			entry, err := io.ReadAll(r)
			if err != nil {
				return false, fmt.Errorf("failed to read leafdata at index %d: %w", begin, err)
			}

			if err := f(end, entry); err != nil {
				return false, err
			}
			end++

			return false, nil
		}()

		if done {
			return end - begin, nil
		}
		if err != nil {
			return end - begin, err
		}
	}
}

// GetObjects returns an object iterator for objects in the entriesDir.
func (c *Client) GetObjects(ctx context.Context, entriesDir string) *gcs.ObjectIterator {
	return c.gcsClient.Bucket(c.bucket).Objects(ctx, &gcs.Query{
		Prefix: entriesDir,
	})
}

// GetObjectData returns the bytes of the input object path.
func (c *Client) GetObjectData(ctx context.Context, obj string) ([]byte, error) {
	r, err := c.gcsClient.Bucket(c.bucket).Object(obj).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetObjectData: failed to create reader for object %q in bucket %q: %q", obj, c.bucket, err)
	}
	defer r.Close()

	return io.ReadAll(r)
}

// Sequence assigns the given leaf entry to the next available sequence number.
// This method will attempt to silently squash duplicate leaves, but it cannot
// be guaranteed that no duplicate entries will exist.
// Returns the sequence number assigned to this leaf (if the leaf has already
// been sequenced it will return the original sequence number and ErrDupeLeaf).
func (c *Client) Sequence(ctx context.Context, leafhash []byte, leaf []byte) (uint64, error) {
	// 1. Check for dupe leafhash
	// 2. Create seq file
	// 3. Create leafhash file containing assigned sequence number

	bkt := c.gcsClient.Bucket(c.bucket)

	// Check for dupe leaf already present.
	leafPath := filepath.Join(layout.LeafPath("", leafhash))
	r, err := bkt.Object(leafPath).NewReader(ctx)
	if err == nil {
		defer r.Close()

		// If there is one, it should contain the existing leaf's sequence number,
		// so read that back and return it.
		seqString, err := io.ReadAll(r)
		if err != nil {
			return 0, err
		}

		origSeq, err := strconv.ParseUint(string(seqString), 16, 64)
		if err != nil {
			return 0, err
		}
		return origSeq, log.ErrDupeLeaf
	} else if !errors.Is(err, gcs.ErrObjectNotExist) {
		return 0, err
	}

	// Now try to sequence it, we may have to scan over some newly sequenced entries
	// if Sequence has been called since the last time an Integrate/WriteCheckpoint
	// was called.
	for {
		seq := c.nextSeq

		// Try to write the sequence file
		seqPath := filepath.Join(layout.SeqPath("", seq))
		if _, err := bkt.Object(seqPath).Attrs(ctx); err == nil {
			// That sequence number is in use, try the next one
			c.nextSeq++
			fmt.Printf("Seq num %d in use, continuing", seq)
			continue
		} else if !errors.Is(err, gcs.ErrObjectNotExist) {
			return 0, fmt.Errorf("couldn't get attr of object %s: %q", seqPath, err)
		}

		// Found the next available sequence number; write it.
		//
		// Conditionally write only if the object does not exist yet:
		// https://cloud.google.com/storage/docs/request-preconditions#special-case.
		// This may exist if there is more than one instance of the sequencer
		// writing to the same log.
		w := bkt.Object(seqPath).If(gcs.Conditions{DoesNotExist: true}).NewWriter(ctx)
		if c.otherCacheControl != "" {
			w.ObjectAttrs.CacheControl = c.otherCacheControl
		}
		if _, err := w.Write(leaf); err != nil {
			return 0, fmt.Errorf("failed to write seq file: %w", err)
		}
		if err := w.Close(); err != nil {
			var e *googleapi.Error
			if ok := errors.As(err, &e); ok {
				// Sequence number already in use.
				if e.Code == http.StatusPreconditionFailed {
					fmt.Printf("GCS writer close failed with sequence number %d: %v. Trying with number %d.\n",
						c.nextSeq, err, c.nextSeq+1)
					c.nextSeq++
					continue
				}
			}

			return 0, fmt.Errorf("couldn't close writer for object %q: %v", seqPath, err)
		}
		fmt.Printf("Wrote leaf data to path %q\n", seqPath)

		// Create a leafhash file containing the assigned sequence number.
		// This isn't infallible though, if we crash after writing the sequence
		// file above but before doing this, a resubmission of the same leafhash
		// would be permitted.
		wLeaf := bkt.Object(leafPath).NewWriter(ctx)
		if c.otherCacheControl != "" {
			w.ObjectAttrs.CacheControl = c.otherCacheControl
		}
		if _, err := wLeaf.Write([]byte(strconv.FormatUint(seq, 16))); err != nil {
			return 0, fmt.Errorf("couldn't create leafhash object: %w", err)
		}
		if err := wLeaf.Close(); err != nil {
			return 0, fmt.Errorf("couldn't close writer for object %q, %w", leafPath, err)
		}

		// All done!
		return seq, nil
	}
}

// assertContent checks that the content at `gcsPath` matches the passed in `data`.
func (c *Client) assertContent(ctx context.Context, gcsPath string, data []byte) (equal bool, err error) {
	bkt := c.gcsClient.Bucket(c.bucket)

	obj := bkt.Object(gcsPath)
	r, err := obj.NewReader(ctx)
	if err != nil {
		klog.V(2).Infof("assertContent: failed to create reader for object %q in bucket %q: %v",
			gcsPath, c.bucket, err)
		return false, err
	}
	defer r.Close()

	gcsData, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}

	if bytes.Equal(gcsData, data) {
		return true, nil
	}
	return false, nil
}

// StoreTile writes a tile out to GCS.
// Fully populated tiles are stored at the path corresponding to the level &
// index parameters, partially populated (i.e. right-hand edge) tiles are
// stored with a .xx suffix where xx is the number of "tile leaves" in hex.
func (c *Client) StoreTile(ctx context.Context, level, index uint64, tile *api.Tile) error {
	tileSize := uint64(tile.NumLeaves)
	klog.V(2).Infof("StoreTile: level %d index %x ts: %x", level, index, tileSize)
	if tileSize == 0 || tileSize > 256 {
		return fmt.Errorf("tileSize %d must be > 0 and <= 256", tileSize)
	}
	t, err := tile.MarshalText()
	if err != nil {
		return fmt.Errorf("failed to marshal tile: %w", err)
	}

	bkt := c.gcsClient.Bucket(c.bucket)

	// Pass an empty rootDir since we don't need this concept in GCS.
	tPath := filepath.Join(layout.TilePath("", level, index, tileSize%256))
	obj := bkt.Object(tPath)

	// Tiles, partial or full, should only be written once.
	w := obj.If(gcs.Conditions{DoesNotExist: true}).NewWriter(ctx)
	if c.otherCacheControl != "" {
		w.ObjectAttrs.CacheControl = c.otherCacheControl
	}
	if _, err := w.Write(t); err != nil {
		return fmt.Errorf("failed to write tile object %q to bucket %q: %w", tPath, c.bucket, err)
	}

	if err := w.Close(); err != nil {
		switch ee := err.(type) {
		case *googleapi.Error:
			// If we run into a precondition failure error, check that the object
			// which exists contains the same content that we want to write.
			if ee.Code == http.StatusPreconditionFailed {
				if equal, err := c.assertContent(ctx, tPath, t); err != nil {
					return fmt.Errorf("failed to read content of %q: %w", tPath, err)
				} else if !equal {
					return fmt.Errorf("assertion that tile content for %q has not changed failed", tPath)
				}

				klog.V(2).Infof("StoreTile: identical tile already exists for level %d index %x ts: %x", level, index, tileSize)
				return nil
			}
		default:
			return err
		}
	}

	return nil
}
