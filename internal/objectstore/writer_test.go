// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/verity"
)

// tempDirEmpty fails the test if any staging temp is left in the store.
func tempDirEmpty(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(s.TempDir())
	if err != nil {
		t.Fatalf("ReadDir temp: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp dir not empty after Close: %v", names)
	}
}

// seedTampered pre-seeds a wrong-bytes, verity-enabled object at the CAS path
// content would commit to, so writing content later collides with it.
func seedTampered(t *testing.T, s *Store, content []byte) {
	t.Helper()
	finalPath := s.objectPath(digest.FromBytes(content).Hex())
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(finalPath) // #nosec G304 -- store-owned test path
	if err != nil {
		t.Fatal(err)
	}
	verr := verity.Enable(f)
	_ = f.Close()
	skipIfNoVerity(t, verr)
	if verr != nil {
		t.Fatalf("seed verity: %v", verr)
	}
}

// TestWriterMatchesSynchronousPath asserts objects written through a Writer,
// duplicates included, land in the store exactly as WriteObject would put
// them: same content and fs-verity digests, view links in place, no temps
// left behind.
func TestWriterMatchesSynchronousPath(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()

	// Probe once synchronously so the test skips cleanly without fs-verity.
	_, _, err := s.WriteObject(ctx, bytes.NewReader([]byte("probe")))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject probe: %v", err)
	}

	contents := make([][]byte, 0, 2*sealQueueDepth+3)
	for i := range 2*sealQueueDepth + 1 {
		contents = append(contents, fmt.Appendf(nil, "object %d", i))
	}
	// Two duplicates: one of a queued object, one of the probe already in CAS.
	contents = append(contents, contents[0], []byte("probe"))

	w := s.NewWriter(ctx)
	type pending struct {
		d      digest.Digest
		sealed Sealed
	}
	got := make([]pending, 0, len(contents))
	for _, c := range contents {
		d, sealed, err := w.WriteObject(ctx, bytes.NewReader(c))
		if err != nil {
			t.Fatalf("WriteObject: %v", err)
		}
		got = append(got, pending{d, sealed})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tempDirEmpty(t, s)

	for i, c := range contents {
		if want := digest.FromBytes(c); got[i].d != want {
			t.Fatalf("object %d digest = %s, want %s", i, got[i].d, want)
		}
		fsv, err := got[i].sealed()
		if err != nil {
			t.Fatalf("object %d Sealed: %v", i, err)
		}
		measured, err := s.VerityDigest(context.Background(), got[i].d)
		if err != nil {
			t.Fatalf("object %d VerityDigest: %v", i, err)
		}
		if fsv == "" || fsv != measured {
			t.Fatalf("object %d fs-verity = %q, measured %q", i, fsv, measured)
		}
		sameInode(t, s.objectPath(got[i].d.Hex()), s.viewPath(fsv[len("sha256:"):]))
		// Resolving again returns the same answer.
		if again, err := got[i].sealed(); err != nil || again != fsv {
			t.Fatalf("object %d Sealed again = %q, %v; want %q", i, again, err, fsv)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestWriterWorkerErrorSurfacesByName asserts a failure inside the worker (a
// tampered object at the staged content's CAS path) is what the batch reports
// everywhere — the failed Sealed, the next WriteObject, Close — rather than
// the cancellation it triggered, and that the failure leaves no temps.
func TestWriterWorkerErrorSurfacesByName(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	authentic := []byte("authentic content")
	seedTampered(t, s, authentic)

	w := s.NewWriter(ctx)
	_, sealed, err := w.WriteObject(ctx, bytes.NewReader(authentic))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	_, sealErr := sealed()
	if sealErr == nil {
		t.Fatal("expected the tampered collision to fail the seal")
	}
	if errors.Is(sealErr, context.Canceled) {
		t.Fatalf("seal error is the cancellation, not the cause: %v", sealErr)
	}

	// The worker has failed and canceled the writer: the next write must fail
	// fast with the same error and leave no temp behind.
	_, _, err = w.WriteObject(ctx, bytes.NewReader([]byte("after failure")))
	if !errors.Is(err, sealErr) {
		t.Fatalf("WriteObject after worker failure = %v, want %v", err, sealErr)
	}
	if err := w.Close(); !errors.Is(err, sealErr) {
		t.Fatalf("Close = %v, want %v", err, sealErr)
	}
	tempDirEmpty(t, s)
	if _, err := os.Stat(s.objectPath(digest.FromBytes([]byte("after failure")).Hex())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object written after the failure was committed (stat err %v)", err)
	}
}

// TestWriterWriteAfterCancelFails asserts that once the writer's context is
// canceled, a write fails with the cancellation and leaves no temp behind,
// while an object sealed before the cancel stays committed.
func TestWriterWriteAfterCancelFails(t *testing.T) {
	s := newVerityStore(t)
	_, _, err := s.WriteObject(context.Background(), bytes.NewReader([]byte("probe")))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject probe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := s.NewWriter(ctx)
	first := []byte("sealed before cancel")
	_, sealed, err := w.WriteObject(ctx, bytes.NewReader(first))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	if _, err := sealed(); err != nil {
		t.Fatalf("Sealed before cancel: %v", err)
	}
	cancel()

	if _, _, err := w.WriteObject(ctx, bytes.NewReader([]byte("after cancel"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteObject after cancel = %v, want context.Canceled", err)
	}
	if err := w.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close after cancel = %v, want context.Canceled", err)
	}
	tempDirEmpty(t, s)
	if _, err := os.Stat(s.objectPath(digest.FromBytes([]byte("after cancel")).Hex())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object staged after cancel was committed (stat err %v)", err)
	}
	if ok, err := s.Has(digest.FromBytes(first)); err != nil || !ok {
		t.Fatalf("Has(first) = %v, %v; want true", ok, err)
	}
}

// TestWriterCancelDiscardsQueue asserts that jobs still queued when the
// context is canceled are discarded, not sealed: Close drains them without
// committing, every pending Sealed reports the cancellation, and no temp
// survives. The writer is built without its worker so the queue is
// deterministically full at the moment of the cancel; the worker starts
// only afterwards.
func TestWriterCancelDiscardsQueue(t *testing.T) {
	s := newVerityStore(t)
	_, _, err := s.WriteObject(context.Background(), bytes.NewReader([]byte("probe")))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject probe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &Writer{s: s, ctx: ctx, cancel: cancel, jobs: make(chan *sealJob, sealQueueDepth)}
	var contents [][]byte
	var pending []Sealed
	for i := range sealQueueDepth / 2 {
		c := fmt.Appendf(nil, "queued %d", i)
		_, sealed, err := w.WriteObject(ctx, bytes.NewReader(c))
		if err != nil {
			t.Fatalf("WriteObject %d: %v", i, err)
		}
		contents = append(contents, c)
		pending = append(pending, sealed)
	}
	cancel()
	w.wg.Add(1)
	go w.run()

	if err := w.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close after cancel = %v, want context.Canceled", err)
	}
	tempDirEmpty(t, s)
	for i, sealed := range pending {
		if _, err := sealed(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sealed %d = %v, want context.Canceled", i, err)
		}
		if ok, err := s.Has(digest.FromBytes(contents[i])); err != nil {
			t.Fatalf("Has %d: %v", i, err)
		} else if ok {
			t.Fatalf("object %d queued at cancel was committed", i)
		}
	}
}

// TestWriterWriteExpected asserts content hashing to the expected digest is
// stored exactly as WriteObject would store it, and that content hashing to
// anything else is rejected while staged: no temp, no object, and the writer
// stays usable.
func TestWriterWriteExpected(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	_, _, err := s.WriteObject(ctx, bytes.NewReader([]byte("probe")))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject probe: %v", err)
	}

	w := s.NewWriter(ctx)
	defer func() { _ = w.Close() }()
	if _, err := w.WriteExpected(ctx, "", bytes.NewReader(nil)); err == nil {
		t.Fatal("WriteExpected accepted an empty expected digest")
	}

	wrong := []byte("not the expected content")
	expected := []byte("the expected content")
	d := digest.FromBytes(expected)
	if _, err := w.WriteExpected(ctx, d, bytes.NewReader(wrong)); err == nil {
		t.Fatal("WriteExpected accepted content with another digest")
	}
	tempDirEmpty(t, s)

	sealed, err := w.WriteExpected(ctx, d, bytes.NewReader(expected))
	if err != nil {
		t.Fatalf("WriteExpected: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tempDirEmpty(t, s)
	fsv, err := sealed()
	if err != nil {
		t.Fatalf("Sealed: %v", err)
	}
	measured, err := s.VerityDigest(context.Background(), d)
	if err != nil {
		t.Fatalf("VerityDigest: %v", err)
	}
	if fsv == "" || fsv != measured {
		t.Fatalf("fs-verity = %q, measured %q", fsv, measured)
	}
	sameInode(t, s.objectPath(d.Hex()), s.viewPath(fsv[len("sha256:"):]))
	if ok, err := s.Has(digest.FromBytes(wrong)); err != nil || ok {
		t.Fatalf("Has(rejected content) = %v, %v; want false", ok, err)
	}
}
