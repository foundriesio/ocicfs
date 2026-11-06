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
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ociseal"
	"github.com/foundriesio/ocicfs/internal/rootfs"
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
// in manifest order; Ingest(i) returns once layer i is merged. A scanned
// layer is saved in the image store, and a layer the store holds a saved
// layer for is merged from it without being read, its stored files pending
// until Finish. The build's own context carries the values of the Begin
// context without its cancellation, so a build may span request handlers;
// Finish and Close end it.
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
	scans    []layerScan   // one per layer, filled by the scan goroutines and, for a reused layer, the launcher
	merged   chan struct{} // closed and replaced on every merge or failure
	launched bool
	closed   bool
	reused   []int // layers published from their saved layers

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
	res, err := prepare(ctx, store, img, src, root, platform, slots)
	if err != nil {
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
// of stream), or merged from its saved layer. A jump forward covers every layer in between; an index at or
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
// resolved platform manifest. When layers were merged from their saved
// layers, it first reads whole, under ctx and the scan limiter, each one that
// lists a file the image needs and the store lacks. On a layer failure or a
// cancelled ctx it stops every scan before returning the error. A Close that
// arrives before Finish starts that tail ends it with ErrClosed, recording
// nothing; one that arrives during it waits for the tail to end. The build is
// spent afterwards.
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
	// Close interrupts nothing below: one arriving now waits for the image
	// and its binding to be written, so the store is never left half-recorded
	// once Close has returned. Only ctx ends a layer read made on the way.
	b.builderMu.Lock()
	defer b.builderMu.Unlock()
	b.mu.Lock()
	closed, reused := b.closed, b.reused
	b.mu.Unlock()
	if closed {
		return Built{}, ErrClosed
	}
	if len(reused) == 0 {
		return finishImage(ctx, b.store, b.img, b.root, b.res, b.b)
	}
	// The build's own context is cancelled by now, so the layers a saved
	// layer leaves to read are read under the caller's.
	missing, err := b.b.Missing(ctx)
	switch {
	case err == nil:
		_, err = supplySaved(ctx, b.store, b.img, b.src, b.b, b.res, reused, missing, b.slots)
	case !cancelled(err):
		removeSaved(b.img, b.res, reused)
	}
	if err != nil {
		// Missing opened the builder's sealing writer, which only the
		// builder's own Finish or Close ends; no Close of the build need follow.
		b.b.Close()
		return Built{}, err
	}
	return finishSaved(ctx, b.store, b.img, b.root, b.res, b.b, reused)
}

// Close abandons a build that will not finish: it stops every scan, waits for
// them, and releases their slots. An Ingest blocked meanwhile returns; a
// Finish past its wait completes first, any layer it reads included, and
// Close returns only once it has.
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
// goroutine that owns the slot the open took. A layer the store holds a
// usable saved layer for is reused instead, within the same window. It stops
// on cancellation or an open failure.
func (b *Build) launch() {
	defer b.wg.Done()
	for j := range b.scans {
		if !b.waitWindow(j) {
			return
		}
		if b.reuse(j) {
			continue
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

// reuse publishes layer j from its saved layer as a finished scan and merges
// whatever prefix of layers is ready, taking no slot and opening no blob. It
// reports false, leaving the layer to be opened, when the store holds no
// saved layer this image can use, or the build is stopped already.
func (b *Build) reuse(j int) bool {
	if b.ctx.Err() != nil {
		return false
	}
	l := loadSaved(b.img, b.res, j)
	if l == nil {
		return false
	}
	b.mu.Lock()
	b.reused = append(b.reused, j)
	b.scans[j] = layerScan{done: true, layer: l}
	b.mu.Unlock()
	b.mergeReady()
	return true
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

// scan scans layer j, saves what a clean scan yielded, publishes the outcome,
// releases the slot it holds, and merges whatever prefix of scanned layers is
// ready.
func (b *Build) scan(j int, layer io.ReadCloser) {
	defer b.wg.Done()
	l, err := b.b.Scan(b.ctx, layer, b.res.DiffIDs[j], j)
	if cerr := layer.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("materialize: layer %d: close: %w", j, cerr))
	}
	if err == nil {
		saveLayer(b.img, b.res, j, l)
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
// has failed. A merge that fails removes the saved layers merged so far, the
// failing one included: one of them may be what a sound layer could not be
// merged over. A saved layer published ahead of the failure was not merged
// and stays.
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
			merged := reusedThrough(b.reused, b.mergedN)
			b.mu.Unlock()
			removeSaved(b.img, b.res, merged)
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

// prepare does what every materialization does before its first layer:
// checks that the store's filesystem can enable fs-verity when this build
// requires it, so a pull fails before writing anything to a store it cannot
// use; resolves root from src for platform; and writes the metadata blobs.
func prepare(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform, slots chan struct{}) (ocichain.Resolved, error) {
	if store == nil {
		return ocichain.Resolved{}, errors.New("materialize: nil object store")
	}
	if img == nil {
		return ocichain.Resolved{}, errors.New("materialize: nil image store")
	}
	if src == nil {
		return ocichain.Resolved{}, errors.New("materialize: nil blob source")
	}
	if cap(slots) == 0 {
		return ocichain.Resolved{}, errors.New("materialize: scan limiter must have a positive capacity")
	}
	if err := store.Preflight(); err != nil {
		return ocichain.Resolved{}, err
	}

	res, err := ocichain.Resolve(ctx, src, root, platform)
	if err != nil {
		return ocichain.Resolved{}, err
	}
	if err := writeMetadataBlobs(ctx, store, res); err != nil {
		return ocichain.Resolved{}, err
	}
	return res, nil
}

// saveLayer saves the scanned layer j of res in img under the layer's blob
// digest, with the blob size and diff_id it was verified against. The caller
// has scanned the whole stream cleanly and has not merged l yet. A layer that
// cannot be saved, or is too large to be, is only read again by a later pull,
// so the failure is logged rather than returned.
func saveLayer(img *imagestore.Store, res ocichain.Resolved, j int, l *rootfs.Layer) {
	desc := res.Manifest.Layers[j]
	data, err := rootfs.EncodeLayer(l, desc.Size, res.DiffIDs[j])
	if err == nil {
		err = img.PutLayer(desc.Digest, data)
	}
	if err != nil {
		slog.Warn("materialize: failed to save layer", "layer", desc.Digest, "err", err)
	}
}

// loadSaved returns layer j of res as its saved layer in img describes it, or
// nil when img holds none this image can use: none at all, or one that does
// not decode or was saved for another blob size or diff_id, which is logged.
func loadSaved(img *imagestore.Store, res ocichain.Resolved, j int) *rootfs.Layer {
	l, err := readSaved(img, res, j)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("materialize: ignoring saved layer", "layer", res.Manifest.Layers[j].Digest, "err", err)
		}
		return nil
	}
	return l
}

// readSaved reads and decodes the saved layer of layer j of res, requiring it
// to have been saved for the blob size the manifest declares and the diff_id
// the config names. A blob with no saved layer surfaces as an error
// satisfying errors.Is(err, os.ErrNotExist).
func readSaved(img *imagestore.Store, res ocichain.Resolved, j int) (*rootfs.Layer, error) {
	desc := res.Manifest.Layers[j]
	data, err := img.GetLayer(desc.Digest)
	if err != nil {
		return nil, err
	}
	return rootfs.DecodeLayer(data, j, desc.Size, res.DiffIDs[j])
}

// removeSaved deletes the saved layers of the layers in reused, so the next
// pull reads those layers from the source. A build calls it for the saved
// layers it merged, on a failure one of them can have caused: of a merge, of
// resolving or supplying a pending file, of building the image, or of its
// sealing verification. A failure to read a layer from the source, a cancellation,
// and a failure to record the built image leave them in place.
func removeSaved(img *imagestore.Store, res ocichain.Resolved, reused []int) {
	for _, j := range reused {
		blob := res.Manifest.Layers[j].Digest
		if err := img.DeleteLayer(blob); err != nil {
			slog.Warn("materialize: failed to remove saved layer", "layer", blob, "err", err)
		}
	}
}

// reusedThrough returns the layers in reused up to and including layer j: the
// saved layers a merge that fails at j has taken in.
func reusedThrough(reused []int, j int) []int {
	var merged []int
	for _, r := range reused {
		if r <= j {
			merged = append(merged, r)
		}
	}
	return merged
}

// supplySaved supplies b with the payloads in missing: files that layers
// merged from their saved layers left pending and the store lacks. Each layer
// assignSaved gives a payload is read whole from src, once, under a slot — a
// scan that stores every file the layer carries and verifies its blob digest
// and diff_id, and whose result is dropped — and each payload is then
// supplied once, out of the store. It returns how many payloads each layer it
// read was read for. A payload that cannot be assigned, or supplied once its
// layer was read, removes the saved layers.
func supplySaved(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, b *rootfs.Builder, res ocichain.Resolved, reused []int, missing []rootfs.Payload, slots chan struct{}) (map[int]int, error) {
	if len(missing) == 0 {
		return nil, nil
	}
	owned, err := assignSaved(img, res, reused, missing)
	if err != nil {
		removeSaved(img, res, reused)
		return nil, err
	}
	for _, j := range reused {
		if owned[j] == 0 {
			continue
		}
		if err := withSlot(ctx, slots, func() error { return rescanLayer(ctx, b, src, res, j) }); err != nil {
			return nil, err
		}
	}
	for _, p := range missing {
		if err := supplyStored(ctx, store, b, p); err != nil {
			if !cancelled(err) {
				removeSaved(img, res, reused)
			}
			return nil, err
		}
	}
	return owned, nil
}

// assignSaved assigns each payload in missing to the first layer in reused
// whose saved layer lists it, and returns the number assigned to each layer
// that got any. The saved layers are read again here rather than kept from
// the merge. A blob the manifest lists twice takes every payload it lists at
// its first position, so no layer is assigned twice. A payload no saved layer
// lists is an error.
func assignSaved(img *imagestore.Store, res ocichain.Resolved, reused []int, missing []rootfs.Payload) (map[int]int, error) {
	unowned := make(map[rootfs.Payload]bool, len(missing))
	for _, p := range missing {
		unowned[p] = true
	}
	owned := make(map[int]int)
	for _, j := range reused {
		if len(unowned) == 0 {
			break
		}
		l, err := readSaved(img, res, j)
		if err != nil {
			return nil, fmt.Errorf("materialize: read the saved layer of layer %d again: %w", j, err)
		}
		for _, p := range l.Payloads() {
			if unowned[p] {
				delete(unowned, p)
				owned[j]++
			}
		}
	}
	for _, p := range missing {
		if unowned[p] {
			return nil, fmt.Errorf("materialize: no saved layer lists missing payload %s of %d bytes", p.Digest, p.Size)
		}
	}
	return owned, nil
}

// rescanLayer reads layer j of res whole from src as a scan does and drops
// the scan's result.
func rescanLayer(ctx context.Context, b *rootfs.Builder, src ocichain.BlobSource, res ocichain.Resolved, j int) error {
	stream, err := ocichain.OpenLayer(ctx, src, res.Manifest.Layers[j])
	if err != nil {
		return fmt.Errorf("materialize: open layer %d: %w", j, err)
	}
	_, err = b.Scan(ctx, stream, res.DiffIDs[j], j)
	if cerr := stream.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("materialize: layer %d: close: %w", j, cerr))
	}
	return err
}

// supplyStored supplies b with the payload p out of the object the store
// holds for it.
func supplyStored(ctx context.Context, store *objectstore.Store, b *rootfs.Builder, p rootfs.Payload) error {
	r, err := store.Open(p.Digest)
	if err != nil {
		return fmt.Errorf("materialize: supply payload %s: %w", p.Digest, err)
	}
	err = b.Supply(ctx, p, r)
	if cerr := r.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if err != nil {
		return fmt.Errorf("materialize: supply payload %s: %w", p.Digest, err)
	}
	return nil
}

// cancelled reports whether err is a context's cancellation or deadline.
func cancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// finishImage finishes the rootfs build, verifies the sealing annotations,
// syncs the store, and records the binding under the resolved platform
// manifest.
func finishImage(ctx context.Context, store *objectstore.Store, img *imagestore.Store, root digest.Digest, res ocichain.Resolved, b *rootfs.Builder) (Built, error) {
	return finishSaved(ctx, store, img, root, res, b, nil)
}

// finishSaved is finishImage for a build that merged the layers in reused
// from their saved layers. When the image cannot be built, or fails its
// sealing verification, for another reason than a cancellation, those saved
// layers are removed.
func finishSaved(ctx context.Context, store *objectstore.Store, img *imagestore.Store, root digest.Digest, res ocichain.Resolved, b *rootfs.Builder, reused []int) (Built, error) {
	built, err := b.Finish(ctx)
	if err != nil {
		if !cancelled(err) {
			removeSaved(img, res, reused)
		}
		return Built{}, err
	}
	if err := ociseal.VerifyAnnotations(ctx, store, res, built.FsverityDigest, built.Canonical); err != nil {
		if !cancelled(err) {
			removeSaved(img, res, reused)
		}
		return Built{}, fmt.Errorf("materialize: verify sealing annotations: %w", err)
	}

	if err := store.Sync(); err != nil {
		return Built{}, err
	}
	if err := img.Put(res.ManifestDesc.Digest, res.Platform, imagestore.Binding{
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootfsSize:     built.RootfsSize,
		Canonical:      built.Canonical,
	}); err != nil {
		return Built{}, err
	}

	return Built{
		Root:           root,
		Platform:       res.Platform,
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootDesc:       res.RootDesc,
		Identity:       res.ManifestDesc,
	}, nil
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
