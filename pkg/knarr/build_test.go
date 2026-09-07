// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/octest"
)

// TestBuildIngestMatchesPullE2E pulls one three-layer image into two fresh
// stores — once with Pull, once through NewBuild, one Ingest per layer, and
// Finish — and requires the resulting object, image-binding, and ref trees to
// be byte-identical: feeding the layers one at a time changes nothing on
// disk.
func TestBuildIngestMatchesPullE2E(t *testing.T) {
	pullStore := verityStoreRoot(t)
	buildStore := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
	ref := "registry.example.com/team/app@" + lay.Root.String()

	fromPull := pullInto(t, pullStore, ref, WithBlobSource(layoutBlobs(t, lay.Dir)))

	eng, err := New(Config{Root: buildStore})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := eng.NewBuild(context.Background(), ref, WithBlobSource(layoutBlobs(t, lay.Dir)))
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer b.Close()
	for i := range lay.DiffIDs {
		if err := b.Ingest(context.Background(), i); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	fromBuild, err := b.Finish(context.Background())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !reflect.DeepEqual(fromBuild, fromPull) {
		t.Fatalf("build image = %#v, pull image = %#v", fromBuild, fromPull)
	}
	assertTreesEqual(t, pullStore, buildStore, "objects")
	assertTreesEqual(t, pullStore, buildStore, "images")
	assertRefsEqual(t, pullStore, buildStore)
}

// TestBuildCachedIngestNoopFinishRecordsRef asserts a build of an image the
// store already holds reads no layer and writes nothing on Ingest, and that
// its Finish records the reference alone, as Pull does on that path.
func TestBuildCachedIngestNoopFinishRecordsRef(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
	pullInto(t, storeRoot, "registry.example.com/team/app@"+lay.Root.String(), WithBlobSource(layoutBlobs(t, lay.Dir)))
	objectsBefore := treeContents(t, storeRoot+"/objects")

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src := &layerOpenLog{src: layoutBlobs(t, lay.Dir), layers: lay.Layers}
	aliasRef := "registry.example.com/team/alias@" + lay.Root.String()
	b, err := eng.NewBuild(context.Background(), aliasRef, WithBlobSource(src))
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer b.Close()
	for i := range lay.DiffIDs {
		if err := b.Ingest(context.Background(), i); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	if len(src.opened) != 0 {
		t.Fatalf("cached build opened layers %v, want none", src.opened)
	}
	if !reflect.DeepEqual(treeContents(t, storeRoot+"/objects"), objectsBefore) {
		t.Fatal("cached build changed the object store on Ingest")
	}

	img, err := b.Finish(context.Background())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if img.Ref != aliasRef || img.Digest != lay.Root.String() {
		t.Fatalf("image = %#v, want ref %q digest %s", img, aliasRef, lay.Root)
	}
	if record := readPullRef(t, storeRoot, aliasRef); record.Resolved.Digest != lay.Root {
		t.Fatalf("alias ref record = %#v, want %q -> %s", record, aliasRef, lay.Root)
	}
}

// TestBuildCallsAfterCloseAndFinishError asserts a build refuses every call
// once it is closed or finished.
func TestBuildCallsAfterCloseAndFinishError(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
	ref := "registry.example.com/team/app@" + lay.Root.String()
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	newBuild := func(t *testing.T) Build {
		t.Helper()
		b, err := eng.NewBuild(context.Background(), ref, WithBlobSource(layoutBlobs(t, lay.Dir)))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		return b
	}
	assertClosed := func(t *testing.T, b Build) {
		t.Helper()
		if err := b.Ingest(context.Background(), 0); !errors.Is(err, errBuildClosed) {
			t.Fatalf("Ingest = %v, want errBuildClosed", err)
		}
		if _, err := b.Finish(context.Background()); !errors.Is(err, errBuildClosed) {
			t.Fatalf("Finish = %v, want errBuildClosed", err)
		}
	}

	t.Run("after Close", func(t *testing.T) {
		b := newBuild(t)
		if err := b.Ingest(context.Background(), 0); err != nil {
			t.Fatalf("Ingest(0): %v", err)
		}
		b.Close()
		assertClosed(t, b)
	})
	t.Run("after Finish", func(t *testing.T) {
		b := newBuild(t)
		defer b.Close()
		if _, err := b.Finish(context.Background()); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		assertClosed(t, b)
	})
}

// TestBuildFailedNewBuildLeavesGCRunnable asserts a NewBuild that fails after
// taking the store lock — here on an option conflict — releases it, so gc is
// not left reporting a busy store for a build nobody can close.
func TestBuildFailedNewBuildLeavesGCRunnable(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "registry.example.com/team/app@sha256:" + strings.Repeat("a", 64)
	_, err = eng.NewBuild(context.Background(), ref, WithBlobSource(&memBlobSource{}), WithFromDir(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("NewBuild error = %v, want the option conflict", err)
	}
	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC after a failed NewBuild: %v", err)
	}
}

// TestBuildFailedFinishReleasesLock asserts a Finish that fails — on a layer
// whose diff_id the config misstates, or on a cancelled context while a layer
// is still being read — releases the store lock, so a following gc succeeds,
// and that Close afterwards is a no-op.
func TestBuildFailedFinishReleasesLock(t *testing.T) {
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())

	t.Run("diff_id mismatch", func(t *testing.T) {
		storeRoot := verityStoreRoot(t)
		src := layoutBlobs(t, lay.Dir)
		root := tamperDiffID(t, src, lay, 2)
		eng, err := New(Config{Root: storeRoot})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+root.String(), WithBlobSource(src))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		if _, err := b.Finish(context.Background()); err == nil || !strings.Contains(err.Error(), "diff_id mismatch") {
			t.Fatalf("Finish error = %v, want diff_id mismatch", err)
		}
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after a failed Finish: %v", err)
		}
		b.Close()
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after Close: %v", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		storeRoot := verityStoreRoot(t)
		src := newGatedBlobSource(layoutBlobs(t, lay.Dir), lay.Layers[0])
		eng, err := New(Config{Root: storeRoot})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+lay.Root.String(), WithBlobSource(src))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := b.Finish(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Finish under a cancelled ctx = %v, want context.Canceled", err)
		}
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after a cancelled Finish: %v", err)
		}
		if _, err := b.Finish(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Finish after the failure = %v, want the first error", err)
		}
		b.Close()
	})
}

// TestBuildGCBusyDuringBuildOKAfterClose asserts an open build holds the
// store against gc for its whole lifetime, downloads included, and that
// Close — once, or repeated — lets gc through.
func TestBuildGCBusyDuringBuildOKAfterClose(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+lay.Root.String(), WithBlobSource(layoutBlobs(t, lay.Dir)))
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	if _, err := eng.GC(context.Background()); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("GC during a build = %v, want ErrStoreBusy", err)
	}
	if err := b.Ingest(context.Background(), 0); err != nil {
		t.Fatalf("Ingest(0): %v", err)
	}
	if _, err := eng.GC(context.Background()); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("GC after an Ingest = %v, want ErrStoreBusy", err)
	}
	b.Close()
	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC after Close: %v", err)
	}
	// A second Close is a no-op: the lock was released once already.
	b.Close()
	if _, err := eng.GC(context.Background()); err != nil {
		t.Fatalf("GC after a repeated Close: %v", err)
	}
}

// TestBuildTamperedDiffIDSticky asserts a layer whose diff_id the config
// misstates fails the Ingest that reaches it and every later call with the
// same error: the build fails closed rather than falling back to a
// whole-image pull that would hit the same fault.
func TestBuildTamperedDiffIDSticky(t *testing.T) {
	forEachWorkers(t, func(t *testing.T, workers int) {
		storeRoot := verityStoreRoot(t)
		useRemoteDefaultTransport(t, failTransport{})
		lay := octest.ThreeLayerImage(t, hostPlatform())
		src := layoutBlobs(t, lay.Dir)
		root := tamperDiffID(t, src, lay, 1)
		eng, err := New(Config{Root: storeRoot, IngestWorkers: workers})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+root.String(), WithBlobSource(src))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		defer b.Close()
		// Layer 1 may be scanned ahead and fail before layer 0 merges, so the
		// failure can already surface at Ingest(0).
		if err := b.Ingest(context.Background(), 0); err != nil && !strings.Contains(err.Error(), "diff_id mismatch") {
			t.Fatalf("Ingest(0) of the intact layer: %v", err)
		}
		first := b.Ingest(context.Background(), 1)
		if first == nil || !strings.Contains(first.Error(), "diff_id mismatch") {
			t.Fatalf("Ingest(1) error = %v, want diff_id mismatch", first)
		}
		if err := b.Ingest(context.Background(), 2); !errors.Is(err, first) {
			t.Fatalf("Ingest(2) after the failure = %v, want the first error %v", err, first)
		}
		if _, err := b.Finish(context.Background()); !errors.Is(err, first) {
			t.Fatalf("Finish after the failure = %v, want the first error %v", err, first)
		}
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after the failed Finish: %v", err)
		}
	})
}

// The interleaving this races for is now prevented by construction: Finish
// holds mu from entry through recordRef, and Close needs that mu before it can
// release the store lock, so it cannot cut in between the image and its
// reference. The test covers both outcomes rather than forcing one, since
// forcing it would mean a hook in production code for a property the mutex
// already guarantees.
//
// TestBuildCloseDuringFinishKeepsLock races Close against a Finish whose
// layers are all merged and asserts the store lock stays with Finish until it
// has recorded whatever it materialized: a gc run once Finish has returned
// either finds the image referenced, or Finish ended with errBuildClosed
// having recorded nothing. A Close landing after the image was materialized
// must not strand it with a binding and no reference.
func TestBuildCloseDuringFinishKeepsLock(t *testing.T) {
	forEachWorkers(t, func(t *testing.T, workers int) {
		storeRoot := verityStoreRoot(t)
		useRemoteDefaultTransport(t, failTransport{})
		lay := octest.ThreeLayerImage(t, hostPlatform())
		ref := "registry.example.com/team/app@" + lay.Root.String()
		eng, err := New(Config{Root: storeRoot, IngestWorkers: workers})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), ref, WithBlobSource(layoutBlobs(t, lay.Dir)))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		if err := b.Ingest(context.Background(), 2); err != nil {
			t.Fatalf("Ingest(2): %v", err)
		}
		type outcome struct {
			img *Image
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			img, err := b.Finish(context.Background())
			done <- outcome{img, err}
		}()
		runtime.Gosched()
		b.Close()
		out := <-done
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after Close: %v", err)
		}
		if out.err != nil {
			if !errors.Is(out.err, errBuildClosed) {
				t.Fatalf("Finish = %v, want success or errBuildClosed", out.err)
			}
			if _, err := os.Stat(octest.RefPath(storeRoot, ref)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Finish returned errBuildClosed but the ref was recorded, stat: %v", err)
			}
			return
		}
		if _, err := eng.Inspect(context.Background(), ref); err != nil {
			t.Fatalf("image gone after a gc that ran once Close returned: %v", err)
		}
	})
}

// TestBuildCloseInterruptsBlockedIngest asserts Close from a second goroutine
// interrupts an Ingest blocked on a layer that is not arriving: the Ingest
// returns errBuildClosed, Close returns while the layer is still held back,
// every later call returns errBuildClosed, and the store lock is released.
func TestBuildCloseInterruptsBlockedIngest(t *testing.T) {
	forEachWorkers(t, func(t *testing.T, workers int) {
		storeRoot := verityStoreRoot(t)
		useRemoteDefaultTransport(t, failTransport{})
		lay := octest.ThreeLayerImage(t, hostPlatform())
		src := newGatedBlobSource(layoutBlobs(t, lay.Dir), lay.Layers[0])
		eng, err := New(Config{Root: storeRoot, IngestWorkers: workers})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+lay.Root.String(), WithBlobSource(src))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}

		ingestDone := make(chan error, 1)
		go func() { ingestDone <- b.Ingest(context.Background(), 0) }()
		<-src.entered
		closeDone := make(chan struct{})
		go func() {
			b.Close()
			close(closeDone)
		}()
		// The gate is never released: a Close that waited for the Ingest to be
		// let through could not return.
		select {
		case <-closeDone:
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not return while an Ingest was blocked")
		}
		if err := <-ingestDone; !errors.Is(err, errBuildClosed) {
			t.Fatalf("interrupted Ingest = %v, want errBuildClosed", err)
		}
		if err := b.Ingest(context.Background(), 1); !errors.Is(err, errBuildClosed) {
			t.Fatalf("Ingest after Close = %v, want errBuildClosed", err)
		}
		if _, err := b.Finish(context.Background()); !errors.Is(err, errBuildClosed) {
			t.Fatalf("Finish after Close = %v, want errBuildClosed", err)
		}
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after a Close during a blocked Ingest: %v", err)
		}
	})
}

// TestBuildIngestCancelledNotSticky asserts an Ingest whose context is
// cancelled while its layer is held back returns context.Canceled and leaves
// the build usable: once the layer is let through, the repeated Ingest and
// Finish succeed and the image is recorded.
func TestBuildIngestCancelledNotSticky(t *testing.T) {
	forEachWorkers(t, func(t *testing.T, workers int) {
		storeRoot := verityStoreRoot(t)
		useRemoteDefaultTransport(t, failTransport{})
		lay := octest.ThreeLayerImage(t, hostPlatform())
		src := newGatedBlobSource(layoutBlobs(t, lay.Dir), lay.Layers[0])
		eng, err := New(Config{Root: storeRoot, IngestWorkers: workers})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+lay.Root.String(), WithBlobSource(src))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		defer b.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ingestDone := make(chan error, 1)
		go func() { ingestDone <- b.Ingest(ctx, 0) }()
		<-src.entered
		cancel()
		if err := <-ingestDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Ingest = %v, want context.Canceled", err)
		}
		close(src.release)
		if err := b.Ingest(context.Background(), 0); err != nil {
			t.Fatalf("Ingest(0) repeated after the cancellation: %v", err)
		}
		img, err := b.Finish(context.Background())
		if err != nil {
			t.Fatalf("Finish after a cancelled Ingest: %v", err)
		}
		if img.Digest != lay.Root.String() {
			t.Fatalf("Finish digest = %s, want %s", img.Digest, lay.Root)
		}
	})
}

// TestBuildCloseKeepsScanFailure asserts that a layer failure the build
// recorded before Close — here a scanned-ahead layer whose diff_id the config
// misstates, with no call in progress to report it — is what every later
// call returns, not errBuildClosed.
func TestBuildCloseKeepsScanFailure(t *testing.T) {
	forEachWorkers(t, func(t *testing.T, workers int) {
		storeRoot := verityStoreRoot(t)
		useRemoteDefaultTransport(t, failTransport{})
		lay := octest.ThreeLayerImage(t, hostPlatform())
		blobs := layoutBlobs(t, lay.Dir)
		root := tamperDiffID(t, blobs, lay, 1)
		src := &closeSignalSource{src: blobs, gate: lay.Layers[1], closed: make(chan struct{})}
		eng, err := New(Config{Root: storeRoot, IngestWorkers: workers})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+root.String(), WithBlobSource(src))
		if err != nil {
			t.Fatalf("NewBuild: %v", err)
		}
		// Layer 1 is scanned ahead of the call for layer 0 and may fail first.
		if err := b.Ingest(context.Background(), 0); err != nil && !strings.Contains(err.Error(), "diff_id mismatch") {
			t.Fatalf("Ingest(0) of the intact layer: %v", err)
		}
		// The scan of layer 1 closes its reader once it has read the stream to
		// its end, where the diff_id check fails; Close then waits for the scan
		// to publish that failure before recording its own.
		<-src.closed
		b.Close()
		for _, call := range []struct {
			name string
			err  error
		}{
			{"Ingest", b.Ingest(context.Background(), 1)},
			{"Finish", func() error { _, err := b.Finish(context.Background()); return err }()},
		} {
			if call.err == nil || !strings.Contains(call.err.Error(), "diff_id mismatch") {
				t.Fatalf("%s after Close = %v, want the layer failure", call.name, call.err)
			}
			if errors.Is(call.err, errBuildClosed) {
				t.Fatalf("%s after Close = %v, want the layer failure over errBuildClosed", call.name, call.err)
			}
		}
		if _, err := eng.GC(context.Background()); err != nil {
			t.Fatalf("GC after Close: %v", err)
		}
	})
}

// forEachWorkers runs fn as a subtest with an engine worker cap of 1, 2 and 4.
func forEachWorkers(t *testing.T, fn func(t *testing.T, workers int)) {
	t.Helper()
	for _, n := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", n), func(t *testing.T) {
			fn(t, n)
		})
	}
}

// tamperDiffID rewrites the image in src so the config's diff_ids[i] names
// content the layer does not have — the layer bytes and their compressed
// digest are untouched, so only the end-of-stream diff_id check can catch it
// — and returns the digest of the rewritten manifest as the new root.
func tamperDiffID(t *testing.T, src *memBlobSource, lay octest.Layout, i int) digest.Digest {
	t.Helper()
	var manifest ocispec.Manifest
	if err := json.Unmarshal(src.blobs[lay.Manifest], &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	var config ocispec.Image
	if err := json.Unmarshal(src.blobs[manifest.Config.Digest], &config); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	config.RootFS.DiffIDs[i] = digest.FromString("not this layer")
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	manifest.Config.Digest = digest.FromBytes(raw)
	manifest.Config.Size = int64(len(raw))
	src.blobs[manifest.Config.Digest] = raw
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	root := digest.FromBytes(raw)
	src.blobs[root] = raw
	return root
}

type (
	// layerOpenLog serves blobs from src and records which layers, by manifest
	// index, were opened.
	layerOpenLog struct {
		src    *memBlobSource
		layers []digest.Digest
		opened []int
	}

	// gatedBlobSource serves blobs from src but parks an Open of the gate
	// digest until release is closed or the Open context is cancelled,
	// closing entered when it parks.
	gatedBlobSource struct {
		src     *memBlobSource
		gate    digest.Digest
		entered chan struct{}
		release chan struct{}
	}

	// closeSignalSource serves blobs from src and closes closed the first
	// time a reader of the gate digest is closed.
	closeSignalSource struct {
		src    *memBlobSource
		gate   digest.Digest
		closed chan struct{}
		once   sync.Once
	}

	// closeSignalReader is the reader closeSignalSource hands out for its
	// gate digest.
	closeSignalReader struct {
		io.ReadCloser
		src *closeSignalSource
	}
)

func (l *layerOpenLog) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	for i, d := range l.layers {
		if d == desc.Digest {
			l.opened = append(l.opened, i)
		}
	}
	return l.src.Open(ctx, desc)
}

func newGatedBlobSource(src *memBlobSource, gate digest.Digest) *gatedBlobSource {
	return &gatedBlobSource{src: src, gate: gate, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedBlobSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest == g.gate {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.src.Open(ctx, desc)
}

func (s *closeSignalSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := s.src.Open(ctx, desc)
	if err != nil || desc.Digest != s.gate {
		return rc, err
	}
	return &closeSignalReader{ReadCloser: rc, src: s}, nil
}

func (r *closeSignalReader) Close() error {
	r.src.once.Do(func() { close(r.src.closed) })
	return r.ReadCloser.Close()
}
