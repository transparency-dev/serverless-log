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

// Package p provides Google Cloud Functions for adding (sequencing and
// integrating) new entries to a serverless log.
package p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gcp_serverless_module/internal/storage"

	kms "cloud.google.com/go/kms/apiv1"
	"github.com/transparency-dev/armored-witness/pkg/kmssigner"
	fmtlog "github.com/transparency-dev/formats/log"
	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/serverless-log/pkg/log"
	"golang.org/x/mod/sumdb/note"
	"google.golang.org/api/iterator"
)

type requestData struct {
	// Common args.
	Origin         string `json:"origin"`
	Bucket         string `json:"bucket"`
	NoteKeyName    string `json:"noteKeyName"`
	KMSKeyRing     string `json:"kmsKeyRing"`
	KMSKeyName     string `json:"kmsKeyName"`
	KMSKeyLocation string `json:"kmsKeyLocation"`
	KMSKeyVersion  uint   `json:"kmsKeyVersion"`

	// Cache-Control header for checkpoint objects
	CheckpointCacheControl string `json:"checkpointCacheControl"`
	// Cache-Control header for non-checkpoint objects
	OtherCacheControl string `json:"otherCacheControl"`

	// For Sequence requests.
	EntriesDir string `json:"entriesDir"`

	// For Integrate requests.
	Initialise bool `json:"initialise"`

	// For Integrate requests.
	CreateBucket bool `json:"createBucket"`
}

func validateCommonArgs(w http.ResponseWriter, d requestData) (ok bool) {
	if len(d.Origin) == 0 {
		http.Error(w, "Please set `origin` in HTTP body to log identifier.", http.StatusBadRequest)
		return false
	}
	if len(d.KMSKeyRing) == 0 {
		http.Error(w, "Please set `kmsKeyRing` in HTTP body to the signing key's key ring.",
			http.StatusBadRequest)
		return false
	}
	if len(d.KMSKeyName) == 0 {
		http.Error(w, "Please set `kmsKeyName` in HTTP body to the signing key's name.",
			http.StatusBadRequest)
		return false
	}
	if len(d.KMSKeyLocation) == 0 {
		http.Error(w, "Please set `kmsKeyLocation` in HTTP body to the signing key's location.",
			http.StatusBadRequest)
		return false
	}
	if d.KMSKeyVersion == 0 {
		http.Error(w, "Please set `kmsKeyVersion` in HTTP body to the signing key's version as an integer.",
			http.StatusBadRequest)
		return false
	}
	if len(d.NoteKeyName) == 0 {
		http.Error(w, "Please set `noteKeyName` in HTTP body to the key name for the note.",
			http.StatusBadRequest)
		return false
	}

	return true
}

// newClient returns a storage Client built for the request args.
func newClient(ctx context.Context, d requestData) (*storage.Client, error) {
	return storage.NewClient(ctx, storage.ClientOpts{
		ProjectID:              os.Getenv("GCP_PROJECT"),
		Bucket:                 d.Bucket,
		CheckpointCacheControl: d.CheckpointCacheControl,
		OtherCacheControl:      d.OtherCacheControl,
	})
}

// Sequence is the entrypoint of the `sequence` GCF function.
func Sequence(w http.ResponseWriter, r *http.Request) {
	// TODO(jayhou): validate that EntriesDir is only touching the log path.

	// process request args

	d := requestData{}
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		fmt.Printf("json.NewDecoder: %v", err)
		http.Error(w, fmt.Sprintf("Failed to decode JSON: %q", err), http.StatusBadRequest)
		return
	}

	if ok := validateCommonArgs(w, d); !ok {
		return
	}
	if len(d.EntriesDir) == 0 {
		http.Error(w, fmt.Sprintf("Please set `entriesDir` in HTTP body to the "+
			"prefix name of the GCS objects in the %q bucket to sequence.", d.Bucket),
			http.StatusBadRequest)
		return
	}

	// init storage

	ctx := r.Context()
	client, err := newClient(ctx, d)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create GCS client: %q", err), http.StatusInternalServerError)
		return
	}

	// Read the current log checkpoint to retrieve next sequence number.

	cpBytes, err := client.ReadCheckpoint(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read log checkpoint: %q", err), http.StatusInternalServerError)
		return
	}

	// Setup KMS note signer and verifier.

	kmClient, _, noteVerifier, err := setupKMS(ctx, w, os.Getenv("GCP_PROJECT"),
		d.KMSKeyLocation, d.KMSKeyRing, d.KMSKeyName, d.KMSKeyVersion, d.NoteKeyName)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer kmClient.Close()

	// Check signatures

	cp, _, _, err := fmtlog.ParseCheckpoint(cpBytes, d.Origin, noteVerifier)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse Checkpoint: %q", err), http.StatusInternalServerError)
		return
	}
	client.SetNextSeq(cp.Size)

	// sequence entries

	h := rfc6962.DefaultHasher
	it := client.GetObjects(ctx, d.EntriesDir)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			http.Error(w,
				fmt.Sprintf("Bucket(%q).Objects: %v", d.Bucket, err),
				http.StatusInternalServerError)
			return
		}
		// Skip this directory - only add files under it.
		if filepath.Clean(attrs.Name) == filepath.Clean(d.EntriesDir) {
			continue
		}

		bytes, err := client.GetObjectData(ctx, attrs.Name)
		fmt.Printf("Sequencing object %q with content %q\n", attrs.Name, string(bytes))
		if err != nil {
			http.Error(w,
				fmt.Sprintf("Failed to get data of object %q: %q", attrs.Name, err),
				http.StatusInternalServerError)
			return
		}

		// ask storage to sequence
		lh := h.HashLeaf(bytes)
		dupe := false
		seq, err := client.Sequence(ctx, lh, bytes)
		if err != nil {
			if errors.Is(err, log.ErrDupeLeaf) {
				dupe = true
			} else {
				http.Error(w,
					fmt.Sprintf("Failed to sequence %q: %q", attrs.Name, err),
					http.StatusInternalServerError)
				return
			}
		}

		l := fmt.Sprintf("Sequence num %d assigned to %s", seq, attrs.Name)
		if dupe {
			l += " (dupe)"
		}
		fmt.Println(l)
	}
}

// setupKMS returns a KeyManagementClient, note signer, note verifier, and
// error. If this function does not return an error, the caller is responsible
// for calling Close() on the KeyManagementClient.
func setupKMS(ctx context.Context, w http.ResponseWriter, gcpProject, keyLocation, keyRing,
	keyName string, keyVersion uint, noteKeyName string) (*kms.KeyManagementClient, note.Signer, note.Verifier, error) {
	kmsKeyName := fmt.Sprintf(kmssigner.KeyVersionNameFormat, gcpProject,
		keyLocation, keyRing, keyName, keyVersion)

	kmClient, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, nil, nil, fmt.Errorf("Failed to create KeyManagementClient: %q", err)
	}

	noteSigner, err := kmssigner.New(ctx, kmClient, kmsKeyName, noteKeyName)
	if err != nil {
		defer kmClient.Close()
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, nil, nil, fmt.Errorf("Failed to instantiate signer: %q", err)
	}

	vkey, err := kmssigner.VerifierKeyString(ctx, kmClient, kmsKeyName, noteSigner.Name())
	if err != nil {
		defer kmClient.Close()
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, nil, nil, fmt.Errorf("Failed to create verifier key string: %q", err)
	}

	noteVerifier, err := note.NewVerifier(vkey)
	if err != nil {
		defer kmClient.Close()
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, nil, nil, fmt.Errorf("Failed to instantiate verifier: %q", err)
	}

	return kmClient, noteSigner, noteVerifier, nil
}

// Integrate is the entrypoint of the `integrate` GCF function.
func Integrate(w http.ResponseWriter, r *http.Request) {
	// process request args

	d := requestData{}
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		fmt.Sprintf("json.NewDecoder: %v\n", err)
		http.Error(w, fmt.Sprintf("Failed to decode JSON: %q", err), http.StatusBadRequest)
		return
	}

	if ok := validateCommonArgs(w, d); !ok {
		return
	}

	// Setup KMS note signer and verifier.
	ctx := r.Context()
	kmClient, noteSigner, noteVerifier, err := setupKMS(ctx, w, os.Getenv("GCP_PROJECT"),
		d.KMSKeyLocation, d.KMSKeyRing, d.KMSKeyName, d.KMSKeyVersion, d.NoteKeyName)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer kmClient.Close()

	client, err := newClient(ctx, d)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create GCS client: %v", err), http.StatusBadRequest)
		return
	}

	var cpNote note.Note
	h := rfc6962.DefaultHasher
	if d.Initialise {
		if d.CreateBucket {
			if err := client.Create(ctx, d.Bucket); err != nil {
				http.Error(w, fmt.Sprintf("Failed to create bucket for log: %v", err), http.StatusBadRequest)
				return
			}
		}

		cp := fmtlog.Checkpoint{
			Hash: h.EmptyRoot(),
		}
		if err := signAndWrite(ctx, &cp, cpNote, noteSigner, client, d.Origin); err != nil {
			http.Error(w, fmt.Sprintf("Failed to sign: %q", err), http.StatusInternalServerError)
		}
		fmt.Fprintf(w, fmt.Sprintf("Initialised log at %s.", d.Bucket))
		return
	}

	// init storage
	cpRaw, err := client.ReadCheckpoint(ctx)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("Failed to read log checkpoint: %q", err),
			http.StatusInternalServerError)
	}

	// Check signatures
	cp, _, _, err := fmtlog.ParseCheckpoint(cpRaw, d.Origin, noteVerifier)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("Failed to open Checkpoint: %q", err),
			http.StatusInternalServerError)
	}

	// Integrate new entries
	newCp, err := log.Integrate(ctx, cp.Size, client, h)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("Failed to integrate: %q", err),
			http.StatusInternalServerError)
		return
	}
	if newCp == nil {
		http.Error(w, "Nothing to integrate", http.StatusInternalServerError)
	}

	err = signAndWrite(ctx, newCp, cpNote, noteSigner, client, d.Origin)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("Failed to sign: %q", err),
			http.StatusInternalServerError)
	}

	return
}

// signAndWrite signs a checkpoint and writes the new checkpoint to GCS.
func signAndWrite(ctx context.Context, cp *fmtlog.Checkpoint, cpNote note.Note,
	s note.Signer, client *storage.Client, origin string) error {
	cp.Origin = origin
	cpNote.Text = string(cp.Marshal())
	cpNoteSigned, err := note.Sign(&cpNote, s)
	if err != nil {
		return fmt.Errorf("failed to sign Checkpoint: %w", err)
	}
	if err := client.WriteCheckpoint(ctx, cpNoteSigned); err != nil {
		return fmt.Errorf("failed to store new log checkpoint: %w", err)
	}
	return nil
}
