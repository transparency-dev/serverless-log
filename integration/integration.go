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
	"testing"

	"github.com/golang/glog"
	"github.com/transparency-dev/merkle"
	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"
	"github.com/transparency-dev/serverless-log/client"
	"github.com/transparency-dev/serverless-log/pkg/log"
	"golang.org/x/mod/sumdb/note"

	fmtlog "github.com/transparency-dev/formats/log"
)

const (
	pubKey            = "astra+cad5a3d2+AZJqeuyE/GnknsCNh1eCtDtwdAwKBddOlS8M2eI1Jt4b"
	privKey           = "PRIVATE+KEY+astra+cad5a3d2+ASgwwenlc0uuYcdy7kI44pQvuz1fw8cS5NqS8RkZBXoy"
	integrationOrigin = "Serverless Integration Test Log"
)

func RunIntegration(t *testing.T, s log.Storage, f client.Fetcher, lh *rfc6962.Hasher) {
	ctx := context.Background()

	// Do a few iterations around the sequence/integrate loop;
	const (
		loops         = 50
		leavesPerLoop = 257
	)

	signer := mustGetSigner(t, privKey)
	// Create signature verifier
	v, err := note.NewVerifier(pubKey)
	if err != nil {
		glog.Exitf("Unable to create new verifier: %q", err)
	}

	lst, err := client.NewLogStateTracker(ctx, f, lh, nil, v, integrationOrigin, client.UnilateralConsensus(f))
	if err != nil {
		t.Fatalf("Failed to create new log state tracker: %q", err)
	}

	for i := 0; i < loops; i++ {
		glog.Infof("----------------%d--------------", i)
		checkpoint := lst.LatestConsistent

		// Sequence some leaves:
		leaves := sequenceNLeaves(ctx, t, s, lh, i*leavesPerLoop, leavesPerLoop)

		var latestCpNote *note.Note
		// Integrate those leaves
		{
			update, err := log.Integrate(ctx, checkpoint.Size, s, lh)
			if err != nil {
				t.Fatalf("Integrate = %v", err)
			}
			update.Origin = integrationOrigin
			cpNote := note.Note{Text: string(update.Marshal())}
			cpNoteSigned, err := note.Sign(&cpNote, signer)
			if err != nil {
				t.Fatalf("Failed to sign Checkpoint: %q", err)
			}
			if err := s.WriteCheckpoint(ctx, cpNoteSigned); err != nil {
				t.Fatalf("Failed to store new log checkpoint: %q", err)
			}
			latestCpNote = &cpNote
		}

		// State tracker will verify consistency of larger tree
		_, _, latestCpRaw, err := lst.Update(ctx)
		if err != nil {
			t.Fatalf("Failed to update tracked log state: %q", err)
		}
		// Verify that the returned checkpoint note is as expected.
		updateNote, err := note.Open(latestCpRaw, note.VerifierList(v))
		if err != nil {
			t.Fatalf("Failed to open checkpoint note returned from Update: %q", err)
		}
		if latestCpNote.Text != updateNote.Text {
			t.Fatalf("LogStateTracker.Update() did not return correct note information. Got %v want %v",
				lst.CheckpointNote.Text, updateNote.Text)
		}
		newCheckpoint := lst.LatestConsistent
		if got, want := newCheckpoint.Size-checkpoint.Size, uint64(leavesPerLoop); got != want {
			t.Errorf("Integrate missed some entries, got %d want %d", got, want)
		}

		pb, err := client.NewProofBuilder(ctx, newCheckpoint, lh.HashChildren, f)
		if err != nil {
			t.Fatalf("Failed to create ProofBuilder: %q", err)
		}

		for _, l := range leaves {
			h := lh.HashLeaf(l)
			idx, err := client.LookupIndex(ctx, f, h)
			if err != nil {
				t.Fatalf("Failed to lookup leaf index: %v", err)
			}
			ip, err := pb.InclusionProof(ctx, idx)
			if err != nil {
				t.Fatalf("Failed to fetch inclusion proof for %d: %v", idx, err)
			}
			if err := proof.VerifyInclusion(lh, idx, newCheckpoint.Size, h, ip, newCheckpoint.Hash); err != nil {
				t.Fatalf("Invalid inclusion proof for %d: %x", idx, ip)
			}
		}
	}
}

func InitialiseStorage(ctx context.Context, t *testing.T, st log.Storage) error {
	t.Helper()
	cp := fmtlog.Checkpoint{}
	cp.Origin = integrationOrigin
	cpNote := note.Note{Text: string(cp.Marshal())}
	s := mustGetSigner(t, privKey)
	cpNoteSigned, err := note.Sign(&cpNote, s)
	if err != nil {
		t.Fatalf("Failed to sign Checkpoint: %q", err)
	}
	if err := st.WriteCheckpoint(ctx, cpNoteSigned); err != nil {
		t.Fatalf("Failed to store new log checkpoint: %q", err)
	}
	return nil
}

func sequenceNLeaves(ctx context.Context, t *testing.T, s log.Storage, lh merkle.LogHasher, start, n int) [][]byte {
	r := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		c := []byte(fmt.Sprintf("Leaf %d", start+i))
		if _, err := s.Sequence(ctx, lh.HashLeaf(c), c); err != nil {
			t.Fatalf("Sequence = %v", err)
		}
		r = append(r, c)
	}
	return r
}

func mustGetSigner(t *testing.T, privKey string) note.Signer {
	t.Helper()
	s, err := note.NewSigner(privKey)
	if err != nil {
		t.Fatalf("Failed to instantiate signer: %q", err)
	}
	return s
}
