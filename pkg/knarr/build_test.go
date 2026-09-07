// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
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
// whose diff_id the config misstates — releases the store lock, so a
// following gc succeeds.
func TestBuildFailedFinishReleasesLock(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
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
}

// TestBuildGCBusyDuringBuildOKAfterClose asserts an open build holds the
// store against gc for its whole lifetime, downloads included, and that
// Close lets gc through.
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
}

// TestBuildTamperedDiffIDSticky asserts a layer whose diff_id the config
// misstates fails the Ingest that reaches it and every later call with the
// same error: the build fails closed rather than falling back to a
// whole-image pull that would hit the same fault.
func TestBuildTamperedDiffIDSticky(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
	src := layoutBlobs(t, lay.Dir)
	root := tamperDiffID(t, src, lay, 1)
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := eng.NewBuild(context.Background(), "registry.example.com/team/app@"+root.String(), WithBlobSource(src))
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer b.Close()
	if err := b.Ingest(context.Background(), 0); err != nil {
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
}

// TestBuildCloseWaitsForInflightIngest asserts Close from a second goroutine
// waits for an Ingest in progress rather than interrupting it: the Ingest
// completes for the caller that asked, and the build is closed afterwards.
func TestBuildCloseWaitsForInflightIngest(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	lay := octest.ThreeLayerImage(t, hostPlatform())
	src := &gatedBlobSource{
		src:     layoutBlobs(t, lay.Dir),
		gate:    lay.Layers[0],
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	eng, err := New(Config{Root: storeRoot})
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
	// released flips just before the Ingest is let through its gate, so a
	// Close that waited for the Ingest holding the build reports true and
	// one that did not can report false. Nothing can prove a goroutine is
	// blocked, so a Close that does not wait is additionally given a
	// bounded time to return while the gate is still shut; a Close that
	// waits cannot take that branch, whatever the scheduling.
	var released atomic.Bool
	closeWaited := make(chan bool, 1)
	go func() {
		b.Close()
		closeWaited <- released.Load()
	}()
	select {
	case <-closeWaited:
		t.Fatal("Close returned while an Ingest was in progress")
	case <-time.After(200 * time.Millisecond):
	}
	released.Store(true)
	close(src.release)

	if err := <-ingestDone; err != nil {
		t.Fatalf("Ingest interrupted by Close: %v", err)
	}
	if !<-closeWaited {
		t.Fatal("Close returned before the Ingest in progress was let through")
	}
	if err := b.Ingest(context.Background(), 1); !errors.Is(err, errBuildClosed) {
		t.Fatalf("Ingest after Close = %v, want errBuildClosed", err)
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
	// digest until release is closed, closing entered when it parks.
	gatedBlobSource struct {
		src     *memBlobSource
		gate    digest.Digest
		entered chan struct{}
		release chan struct{}
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

func (g *gatedBlobSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest == g.gate {
		close(g.entered)
		<-g.release
	}
	return g.src.Open(ctx, desc)
}
