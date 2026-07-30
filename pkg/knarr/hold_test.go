// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/holdstore"
	"github.com/foundriesio/knarr/internal/octest"
)

func TestEngineHoldThenRemoveDeclines(t *testing.T) {
	root := t.TempDir()
	eng, imageRoot, ref := seedHeldImage(t, root)

	if err := eng.Hold(context.Background(), ref, "cli"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	err := eng.Remove(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "held by cli") {
		t.Fatalf("Remove while held error = %v, want it to name the holder", err)
	}
	// The ref and binding survive a declined removal.
	if _, err := newRefStore(t, root).Get(ref); err != nil {
		t.Fatalf("ref removed by a declined remove: %v", err)
	}
	assertHolders(t, root, imageRoot, []string{"cli"})
}

func TestEngineReleaseThenRemoveSucceeds(t *testing.T) {
	root := t.TempDir()
	eng, imageRoot, ref := seedHeldImage(t, root)

	if err := eng.Hold(context.Background(), ref, "cli"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := eng.Release(context.Background(), ref, "cli"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertHolders(t, root, imageRoot, nil)
	if err := eng.Remove(context.Background(), ref); err != nil {
		t.Fatalf("Remove after release: %v", err)
	}
}

func TestEngineTwoHoldersRemoveDeclinesUntilLast(t *testing.T) {
	root := t.TempDir()
	eng, _, ref := seedHeldImage(t, root)

	if err := eng.Hold(context.Background(), ref, "cli"); err != nil {
		t.Fatalf("Hold cli: %v", err)
	}
	if err := eng.Hold(context.Background(), ref, "cfs"); err != nil {
		t.Fatalf("Hold cfs: %v", err)
	}
	if err := eng.Remove(context.Background(), ref); err == nil {
		t.Fatal("Remove with two holders: got nil, want a decline")
	}
	if err := eng.Release(context.Background(), ref, "cli"); err != nil {
		t.Fatalf("Release cli: %v", err)
	}
	if err := eng.Remove(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "held by cfs") {
		t.Fatalf("Remove with one holder left error = %v, want it to name cfs", err)
	}
	if err := eng.Release(context.Background(), ref, "cfs"); err != nil {
		t.Fatalf("Release cfs: %v", err)
	}
	if err := eng.Remove(context.Background(), ref); err != nil {
		t.Fatalf("Remove after last release: %v", err)
	}
}

func TestEngineForceRemovesAndClears(t *testing.T) {
	root := t.TempDir()
	eng, imageRoot, ref := seedHeldImage(t, root)

	if err := eng.Hold(context.Background(), ref, "cli"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := eng.Remove(context.Background(), ref, WithForce()); err != nil {
		t.Fatalf("forced Remove: %v", err)
	}
	assertHolders(t, root, imageRoot, nil)
}

func TestEngineHoldNeverPulledIsNotFound(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	absent := digest.SHA256.FromString("absent").String()
	err = eng.Hold(context.Background(), absent, "cli")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "knarr pull") {
		t.Fatalf("Hold of a never-pulled digest error = %v, want ErrNotFound with a pull hint", err)
	}
}

func TestEngineHoldRejectsBadHolder(t *testing.T) {
	root := t.TempDir()
	eng, _, ref := seedHeldImage(t, root)

	if err := eng.Hold(context.Background(), ref, "bad/holder"); err == nil {
		t.Fatal("Hold with a bad holder name: got nil, want rejection")
	}
}

func TestEngineBareDigestHold(t *testing.T) {
	root := t.TempDir()
	eng, imageRoot, _ := seedHeldImage(t, root)

	// A hold by bare digest skips ref resolution, so it works regardless of
	// which ref (if any) names the image.
	if err := eng.Hold(context.Background(), imageRoot.String(), "cfs"); err != nil {
		t.Fatalf("bare-digest Hold: %v", err)
	}
	assertHolders(t, root, imageRoot, []string{"cfs"})
}

func TestEngineBareDigestHoldObjectOnly(t *testing.T) {
	root := t.TempDir()
	// A plain content object is present — no ref, no binding, and nothing that
	// parses as OCI metadata to descend. Presence is decided by the object
	// store, so a bare-digest hold of content-in-store succeeds.
	imageRoot, _ := writeEngineObject(t, root, []byte("loose object content"))
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := eng.Hold(context.Background(), imageRoot.String(), "cfs"); err != nil {
		t.Fatalf("bare-digest Hold of object-only image: %v", err)
	}
	assertHolders(t, root, imageRoot, []string{"cfs"})
}

// TestEngineHoldRemoveConcurrent runs Hold and a plain (non-force) Remove of the
// same image concurrently and asserts the per-digest lock serializes them into
// one of two clean end states, never a torn one. Remove forgets the ref and
// binding but not the object, so the root content stays present either way and
// Hold always records its holder; the outcomes differ only in whether Remove
// declined (ref intact) or won (ref gone).
func TestEngineHoldRemoveConcurrent(t *testing.T) {
	for i := 0; i < 50; i++ {
		root := t.TempDir()
		eng, imageRoot, ref := seedHeldImage(t, root)

		var (
			wg                 sync.WaitGroup
			holdErr, removeErr error
		)
		wg.Add(2)
		go func() { defer wg.Done(); holdErr = eng.Hold(context.Background(), ref, "cfs") }()
		go func() { defer wg.Done(); removeErr = eng.Remove(context.Background(), ref) }()
		wg.Wait()

		// The object always survives, so Hold always records its holder.
		if holdErr != nil {
			t.Fatalf("iteration %d: Hold err = %v, want nil (content stays present)", i, holdErr)
		}
		assertHolders(t, root, imageRoot, []string{"cfs"})

		_, refErr := newRefStore(t, root).Get(ref)
		refPresent := refErr == nil
		if refPresent {
			// Remove acquired the lock first? No — if the ref is intact, Remove must
			// have seen the holder and declined by name.
			if removeErr == nil || !strings.Contains(removeErr.Error(), "held by cfs") {
				t.Fatalf("iteration %d: ref present but Remove err = %v, want a decline naming cfs", i, removeErr)
			}
		} else {
			// Ref gone: Remove ran fully before Hold and reported success.
			if removeErr != nil {
				t.Fatalf("iteration %d: ref gone but Remove err = %v, want nil", i, removeErr)
			}
		}
	}
}

// TestEngineHoldContextCanceledUnderLock proves the under-lock context recheck
// stops a hold before it records anything. A separate holdstore instance holds
// the per-digest lock (a distinct open file description), so the engine's Hold
// blocks on Lock until released; with the context already canceled, the recheck
// that runs after acquisition must return the cancellation and write no holder.
func TestEngineHoldContextCanceledUnderLock(t *testing.T) {
	root := t.TempDir()
	eng, imageRoot, ref := seedHeldImage(t, root)

	blocker, err := holdstore.New(root)
	if err != nil {
		t.Fatalf("holdstore.New: %v", err)
	}
	unlock, err := blocker.Lock(imageRoot)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Hold(ctx, ref, "cli") }()

	// The Hold is blocked on the engine's Lock; release ours so it acquires and
	// reaches the under-lock recheck.
	unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Hold err = %v, want context.Canceled", err)
	}
	// The recheck fired before the marker write: no holder recorded.
	assertHolders(t, root, imageRoot, nil)
}

// TestEngineRemoveContextCanceledUnderLock proves the under-lock recheck stops a
// forced remove before it clears holds or deletes state.
func TestEngineRemoveContextCanceledUnderLock(t *testing.T) {
	root := t.TempDir()
	eng, imageRoot, ref := seedHeldImage(t, root)
	if err := eng.Hold(context.Background(), ref, "cfs"); err != nil {
		t.Fatalf("seed Hold: %v", err)
	}

	blocker, err := holdstore.New(root)
	if err != nil {
		t.Fatalf("holdstore.New: %v", err)
	}
	unlock, err := blocker.Lock(imageRoot)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Remove(ctx, ref, WithForce()) }()

	unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove err = %v, want context.Canceled", err)
	}
	// The recheck fired before the force-clear and the deletes: the holder, the
	// ref, and the binding all survive.
	assertHolders(t, root, imageRoot, []string{"cfs"})
	if _, err := newRefStore(t, root).Get(ref); err != nil {
		t.Fatalf("ref deleted despite cancellation: %v", err)
	}
	if _, err := newImageStore(t, root).Get(imageRoot, hostPlatform()); err != nil {
		t.Fatalf("binding deleted despite cancellation: %v", err)
	}
}

// seedHeldImage writes the state the hold verbs and remove treat as a pulled
// image: a real host-platform image manifest in the object store (the
// authoritative presence signal, which the verbs read and descend), plus a ref
// and host-platform binding for remove to delete. It returns the engine, the
// image's identity digest, and the ref naming it.
func seedHeldImage(t *testing.T, root string) (Engine, digest.Digest, string) {
	t.Helper()
	lay := seedChainImage(t, root, "registry.example.com/app@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
	}, nil)
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng, lay.Root, "registry.example.com/app@" + lay.Root.String()
}

// assertHolders checks the holders recorded for imageRoot equal want (nil for
// none).
func assertHolders(t *testing.T, root string, imageRoot digest.Digest, want []string) {
	t.Helper()
	holds, err := holdstore.New(root)
	if err != nil {
		t.Fatalf("holdstore.New: %v", err)
	}
	unlock, err := holds.Lock(imageRoot)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()
	got, err := holds.Holders(imageRoot)
	if err != nil {
		t.Fatalf("Holders: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Holders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Holders = %v, want %v", got, want)
		}
	}
}
