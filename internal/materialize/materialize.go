// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package materialize builds and records a prepared image from verified OCI
// blobs.
package materialize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/rootfs"
)

// Built is the image data materialized into the local stores. Root and RootDesc
// name the root the caller pinned, which may be an image index; Identity names
// the platform image manifest that root resolved to, the descriptor the binding
// is keyed by.
type Built struct {
	Root           digest.Digest
	Platform       ocispec.Platform
	Descriptor     ocispec.Descriptor
	FsverityDigest string
	RootDesc       ocispec.Descriptor
	Identity       ocispec.Descriptor
}

// Build is one image being materialized a layer at a time. Layers are scanned
// ahead of the caller's Ingest calls under the engine's worker cap and merged
// in manifest order; Ingest(i) returns once layer i is merged. The build's own
// context carries the values of the Begin context without its cancellation,
// so a build may span request handlers; Finish and Close end it.
type Build struct {
	store *objectstore.Store
	img   *imagestore.Store
	src   ocichain.BlobSource
	root  digest.Digest
	res   ocichain.Resolved
	b     *rootfs.Builder

	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{} // the caller's engine-wide scan limiter
	window int           // scanned-but-unmerged layers a build may run ahead

	mu       sync.Mutex
	accepted int           // highest layer index an Ingest or Finish asked for; -1 before the first
	mergedN  int           // layers merged so far, in index order
	err      error         // first causative failure
	scans    []layerScan   // one per layer, filled by the scan goroutines
	merged   chan struct{} // closed and replaced on every merge or failure
	launched bool
	closed   bool

	builderMu sync.Mutex // serializes calls on b.b: Merge, and Finish's tail against Close
	wg        sync.WaitGroup
}

// layerScan is the outcome of one layer's scan, published under Build.mu.
type layerScan struct {
	done  bool
	layer *rootfs.Layer
	err   error
}

// ErrClosed reports a call that Close stopped on a build with no layer
// failure to report instead.
var ErrClosed = errors.New("materialize: build is closed")

// retryAbsentLayer is the interval at which a layer that failed to open is
// opened again while the caller has not asked for it.
const retryAbsentLayer = 250 * time.Millisecond

// Begin does everything Materialize does before the first layer: verifies the
// store can enable fs-verity, resolves root from src for platform, writes the
// metadata blobs, and creates the rootfs builder. ctx governs this call's
// reads only; its values, but not its cancellation, carry over to the build.
// slots caps the layer scans running at once across every build sharing it.
// The caller must Finish or Close the build.
func Begin(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform, slots chan struct{}) (*Build, error) {
	if store == nil {
		return nil, errors.New("materialize: nil object store")
	}
	if img == nil {
		return nil, errors.New("materialize: nil image store")
	}
	if src == nil {
		return nil, errors.New("materialize: nil blob source")
	}
	if cap(slots) == 0 {
		return nil, errors.New("materialize: scan limiter must have a positive capacity")
	}
	if err := store.CheckVerity(); err != nil {
		return nil, err
	}

	res, err := ocichain.Resolve(ctx, src, root, platform)
	if err != nil {
		return nil, err
	}
	if err := writeMetadataBlobs(ctx, store, res); err != nil {
		return nil, err
	}
	bctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &Build{
		store: store, img: img, src: src, root: root, res: res, b: rootfs.NewBuilder(store),
		ctx: bctx, cancel: cancel, slots: slots, window: 2 * cap(slots),
		accepted: -1, scans: make([]layerScan, len(res.DiffIDs)), merged: make(chan struct{}),
	}, nil
}

// Ingest returns once every layer up to and including i is merged, in
// manifest order: each is opened through the blob source (its compressed
// digest verified as it is consumed) and scanned (its diff_id verified at end
// of stream). A jump forward covers every layer in between; an index at or
// below one already merged returns at once; an index outside the manifest's
// layers is an error. A failure of any layer scanned so far is returned here
// and by every later call; a cancelled ctx returns its error and leaves the
// build running, so the call may be repeated, and one cancelled before it
// starts asks for no layer.
func (b *Build) Ingest(ctx context.Context, i int) error {
	if i < 0 || i >= len(b.res.DiffIDs) {
		return fmt.Errorf("materialize: layer index %d out of range for an image with %d layers", i, len(b.res.DiffIDs))
	}
	// A call cancelled already must not mark layer i as asked for, which
	// would make a failure to open it the build's rather than a retry.
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	if i > b.accepted {
		b.accepted = i
	}
	b.start()
	b.mu.Unlock()
	return b.waitMerged(ctx, i+1)
}

// Finish waits for every layer to merge, finishes the rootfs build, verifies
// the sealing annotations, syncs the store, and records the binding under the
// resolved platform manifest. On a layer failure or a cancelled ctx it stops
// every scan before returning the error. A Close that arrives before Finish
// starts writing ends it with ErrClosed, recording nothing; one that
// arrives while it writes waits for the image and its binding to land. The
// build is spent afterwards.
func (b *Build) Finish(ctx context.Context) (Built, error) {
	n := len(b.res.DiffIDs)
	b.mu.Lock()
	b.accepted = n - 1
	b.start()
	b.mu.Unlock()
	err := b.waitMerged(ctx, n)
	// Nothing is left to scan on success, and on failure nothing may keep
	// writing to the store once the caller regains control.
	b.cancel()
	b.wg.Wait()
	if err != nil {
		// A layer failure published while the caller's ctx was being
		// cancelled is the failure worth reporting.
		b.mu.Lock()
		if b.err != nil {
			err = b.err
		}
		b.mu.Unlock()
		return Built{}, err
	}
	// Nothing below is interruptible: a Close arriving now waits for the
	// image and its binding to be written, so the store is never left
	// half-recorded once Close has returned.
	b.builderMu.Lock()
	defer b.builderMu.Unlock()
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return Built{}, ErrClosed
	}
	built, err := b.b.Finish(ctx)
	if err != nil {
		return Built{}, err
	}
	if err := ociseal.VerifyAnnotations(ctx, b.store, b.res, built.FsverityDigest, built.Canonical); err != nil {
		return Built{}, fmt.Errorf("materialize: verify sealing annotations: %w", err)
	}

	if err := b.store.Sync(); err != nil {
		return Built{}, err
	}
	if err := b.img.Put(b.res.ManifestDesc.Digest, b.res.Platform, imagestore.Binding{
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootfsSize:     built.RootfsSize,
		Canonical:      built.Canonical,
	}); err != nil {
		return Built{}, err
	}

	return Built{
		Root:           b.root,
		Platform:       b.res.Platform,
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootDesc:       b.res.RootDesc,
		Identity:       b.res.ManifestDesc,
	}, nil
}

// Close abandons a build that will not finish: it stops every scan, waits for
// them, and releases their slots. An Ingest blocked meanwhile returns; a
// Finish past its wait completes first, and Close returns only once it has.
// Objects already sealed stay in the store as unreferenced content. It
// returns the first layer failure the build recorded, if any, so a caller can
// report it over its own closed state. Idempotent; a no-op after Finish.
func (b *Build) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
	b.builderMu.Lock()
	b.b.Close()
	b.builderMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// start launches the scan-ahead goroutine once. The caller holds mu.
func (b *Build) start() {
	if b.launched || b.closed {
		return
	}
	b.launched = true
	b.wg.Add(1)
	go b.launch()
}

// waitMerged blocks until want layers are merged, the build fails or is
// closed, or ctx is cancelled. A recorded failure is reported even when the
// wanted layers are already merged, so no call succeeds on a failed build.
func (b *Build) waitMerged(ctx context.Context, want int) error {
	for {
		b.mu.Lock()
		if err := b.failure(); err != nil {
			b.mu.Unlock()
			return err
		}
		if b.mergedN >= want {
			b.mu.Unlock()
			return nil
		}
		wake := b.merged
		b.mu.Unlock()
		select {
		case <-wake:
		case <-b.ctx.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// failure is the error a waiter returns: the first layer failure, or the
// closed sentinel once the build was cancelled without one. The caller holds
// mu.
func (b *Build) failure() error {
	if b.err != nil {
		return b.err
	}
	if b.ctx.Err() != nil {
		return ErrClosed
	}
	return nil
}

// launch opens the layers in index order, each once the build is within its
// window of merged layers and a slot is free, and hands each to a scan
// goroutine that owns the slot the open took. It stops on cancellation or an
// open failure.
func (b *Build) launch() {
	defer b.wg.Done()
	for j := range b.scans {
		if !b.waitWindow(j) {
			return
		}
		layer, err := b.openLayer(j)
		if err != nil {
			b.mu.Lock()
			b.scans[j] = layerScan{done: true, err: err}
			b.fail(err)
			b.mu.Unlock()
			return
		}
		b.wg.Add(1)
		go b.scan(j, layer)
	}
}

// waitWindow blocks until layer j is within the window of merged layers,
// reporting false once the build is cancelled instead.
func (b *Build) waitWindow(j int) bool {
	for {
		b.mu.Lock()
		if j-b.mergedN < b.window {
			b.mu.Unlock()
			return true
		}
		wake := b.merged
		b.mu.Unlock()
		select {
		case <-wake:
		case <-b.ctx.Done():
			return false
		}
	}
}

// openLayer takes a worker slot and opens layer j through the blob source,
// returning the layer with that slot held: the scan it is handed to releases
// it. An open that fails releases the slot before anything else, so a layer
// that is not there yet never occupies one. While the caller has not asked
// for that layer the blob may still be arriving, and sources differ in how
// they report one they do not hold, so any failure is retried after a pause;
// once the caller has asked for it, the failure is the build's.
//
// Whether the layer has been asked for is read before the attempt, not after.
// A request arriving while the source is being called must not make that
// attempt's failure final: the layer may have appeared in the same window, and
// reporting the stale error would fail a build whose blob is now there. The
// next turn of the loop reads the flag again, so an asked-for layer that is
// still missing fails one pause later.
func (b *Build) openLayer(j int) (io.ReadCloser, error) {
	for {
		b.mu.Lock()
		asked := j <= b.accepted
		b.mu.Unlock()
		select {
		case b.slots <- struct{}{}:
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		}
		layer, err := ocichain.OpenLayer(b.ctx, b.src, b.res.Manifest.Layers[j])
		if err == nil {
			return layer, nil
		}
		<-b.slots
		if asked {
			return nil, fmt.Errorf("materialize: open layer %d: %w", j, err)
		}
		t := time.NewTimer(retryAbsentLayer)
		select {
		case <-t.C:
		case <-b.ctx.Done():
			t.Stop()
			return nil, b.ctx.Err()
		}
	}
}

// scan scans layer j, publishes the outcome, releases the slot it holds, and
// merges whatever prefix of scanned layers is ready.
func (b *Build) scan(j int, layer io.ReadCloser) {
	defer b.wg.Done()
	l, err := b.b.Scan(b.ctx, layer, b.res.DiffIDs[j], j)
	if cerr := layer.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("materialize: layer %d: close: %w", j, cerr))
	}
	b.mu.Lock()
	b.scans[j] = layerScan{done: true, layer: l, err: err}
	if err != nil {
		b.fail(err)
	}
	b.mu.Unlock()
	<-b.slots
	b.mergeReady()
}

// fail records err as the build's failure and stops every other scan. An
// error the cancellation of a stopped build induced is not the failure: the
// first one recorded already is, or the caller's own Close or cancelled
// Finish. The caller holds mu.
func (b *Build) fail(err error) {
	induced := (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && b.ctx.Err() != nil
	if b.err == nil && !induced {
		b.err = err
	}
	b.wake()
	b.cancel()
}

// wake releases every waiter on merged. The caller holds mu.
func (b *Build) wake() {
	close(b.merged)
	b.merged = make(chan struct{})
}

// mergeReady merges, in index order, every scanned layer that follows the
// merged prefix, stopping at the first one still scanning or once the build
// has failed.
func (b *Build) mergeReady() {
	b.builderMu.Lock()
	defer b.builderMu.Unlock()
	for {
		b.mu.Lock()
		if b.failure() != nil || b.mergedN == len(b.scans) {
			b.mu.Unlock()
			return
		}
		s := b.scans[b.mergedN]
		// The error check is redundant with the failure() above — a scan that
		// records an error calls fail under this same lock — and is kept so a
		// slot that failed can never reach Merge with no layer to merge.
		if !s.done || s.err != nil {
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		err := b.b.Merge(s.layer)
		b.mu.Lock()
		if err != nil {
			b.fail(err)
			b.mu.Unlock()
			return
		}
		b.scans[b.mergedN].layer = nil
		b.mergedN++
		b.wake()
		b.mu.Unlock()
	}
}

// Materialize resolves root from src, builds the rootfs image, and records its
// binding in img under the resolved platform manifest, the digest that
// identifies the image however the caller named it. It is Begin followed by
// Finish, scanning layers under the slots limiter.
func Materialize(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform, slots chan struct{}) (Built, error) {
	b, err := Begin(ctx, store, img, src, root, platform, slots)
	if err != nil {
		return Built{}, err
	}
	defer b.Close() //nolint:errcheck // Finish reported any failure already
	return b.Finish(ctx)
}

func writeMetadataBlobs(ctx context.Context, store *objectstore.Store, res ocichain.Resolved) error {
	seen := make(map[digest.Digest]bool, 3)
	if err := writeMetaBlob(ctx, store, seen, res.RootDesc, res.RootBytes); err != nil {
		return fmt.Errorf("write root metadata: %w", err)
	}
	if err := writeMetaBlob(ctx, store, seen, res.ManifestDesc, res.ManifestBytes); err != nil {
		return fmt.Errorf("write manifest metadata: %w", err)
	}
	if err := writeMetaBlob(ctx, store, seen, res.ConfigDesc, res.ConfigBytes); err != nil {
		return fmt.Errorf("write config metadata: %w", err)
	}
	return nil
}

func writeMetaBlob(ctx context.Context, store *objectstore.Store, seen map[digest.Digest]bool, desc ocispec.Descriptor, data []byte) error {
	if seen[desc.Digest] {
		return nil
	}
	seen[desc.Digest] = true
	return store.WriteBlob(ctx, desc.Digest, bytes.NewReader(data))
}
