// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package materialize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/rootfs"
	"github.com/foundriesio/knarr/internal/zstdchunked"
)

// The bounds within which a layer's missing files are fetched as byte ranges;
// a layer whose plan exceeds them is fetched whole.
var (
	// coalesceGap is the widest gap between two files' extents that one range
	// request spans rather than two.
	coalesceGap int64 = 64 << 10
	// maxDeltaRequests is the most range requests one layer may take.
	maxDeltaRequests = 256
	// maxDeltaFraction is the largest share of a layer blob the ranges may
	// add up to.
	maxDeltaFraction = 0.5
)

// ErrNoSpace reports that the store's filesystem cannot hold the content a
// delta pull would fetch.
var ErrNoSpace = errors.New("materialize: not enough space in the store")

// The ways a layer's bytes arrive, as the per-layer log line reports them.
const (
	fallbackNone     = "none"         // the planned ranges
	fallbackRequests = "requests"     // the whole blob: the plan needed too many requests
	fallbackFraction = "fraction"     // the whole blob: the plan covered too much of it
	fallbackWhole    = "whole-answer" // the whole blob: the source answered a range with it
	fallbackNoTOC    = "no-toc"       // the tar stream: the layer has no table of contents
	fallbackTOCError = "toc-error"    // the tar stream: the table of contents could not be read
)

// deltaLayer is one layer of a delta pull: its table of contents, how its
// bytes arrive, and the figures the pull reports about it.
type deltaLayer struct {
	idx  int
	desc ocispec.Descriptor
	// toc is nil for a layer scanned from its tar stream, which scanned then
	// holds.
	toc     *zstdchunked.TOC
	scanned *rootfs.Layer
	// spool holds the whole verified blob when the source answered the table
	// of contents request with it; files are read out of the spool.
	spool *os.File
	// files are the missing payloads this layer supplies, in offset order.
	files []fileFetch
	// requests are the files' extents coalesced into byte ranges.
	requests []byteRange
	reason   string

	tocBytes   int64
	tocLatency time.Duration
	planned    int64
	fetched    int64
}

// fileFetch is one missing payload and the entry whose extent supplies it.
type fileFetch struct {
	payload rootfs.Payload
	entry   zstdchunked.Entry
}

// byteRange is one range request and the files it carries.
type byteRange struct {
	off, end int64
	files    []fileFetch
}

// Delta materializes an image as Materialize does but fetches, of every
// layer with a zstd:chunked table of contents, only the table and the files
// the store lacks, as byte ranges served by ranges. Every layer's table is
// fetched under the slots limiter, and a layer whose table cannot be read,
// or that has none, is scanned from its tar stream in the same turn. A layer
// whose missing files would take too many or too large ranges is read whole
// instead, under the limiter too, while the layers fetched by range take
// their ranges one at a time on the calling goroutine, under the limiter as
// well; either way the image built is the one Materialize builds.
func Delta(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, ranges ocichain.RangeSource, root digest.Digest, platform ocispec.Platform, slots chan struct{}) (Built, error) {
	if ranges == nil {
		return Built{}, errors.New("materialize: nil range source")
	}
	res, err := prepare(ctx, store, img, src, root, platform, slots)
	if err != nil {
		return Built{}, err
	}
	b := rootfs.NewBuilder(store)
	defer b.Close()

	// The layers are read under one cancellation: the first failure stops the
	// rest, and every goroutine is joined before Delta returns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	fail := func(err error) {
		mu.Lock()
		if failed == nil {
			failed = err
		}
		mu.Unlock()
		cancel()
	}
	// under runs fn on l under a slot, unless the pull was cancelled first,
	// and records its failure. A cancellation is recorded like any error: the
	// first failure recorded is the one reported, so a cancellation that a
	// failure caused never replaces it, and one the source raised on its own
	// is not lost.
	under := func(l *deltaLayer, fn func(*deltaLayer) error) {
		defer wg.Done()
		if err := withSlot(ctx, slots, func() error { return fn(l) }); err != nil {
			fail(err)
		}
	}
	// wait joins every goroutine and returns the first failure, or the
	// cancellation that stopped them.
	wait := func() error {
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		if failed != nil {
			return failed
		}
		return ctx.Err()
	}

	layers := make([]*deltaLayer, len(res.Manifest.Layers))
	for j, desc := range res.Manifest.Layers {
		layers[j] = &deltaLayer{idx: j, desc: desc}
		wg.Add(1)
		go under(layers[j], func(l *deltaLayer) error { return readLayer(ctx, b, src, ranges, store, res, l) })
	}
	defer removeSpools(layers)
	if err := wait(); err != nil {
		return Built{}, err
	}

	for _, l := range layers {
		if err := mergeLayer(b, l); err != nil {
			return Built{}, err
		}
	}
	missing, err := b.Missing(ctx)
	if err != nil {
		return Built{}, err
	}
	if err := checkSpace(store, missing); err != nil {
		return Built{}, err
	}
	if err := plan(layers, missing); err != nil {
		return Built{}, err
	}

	for _, l := range layers {
		if l.reason == fallbackNone || len(l.files) == 0 {
			continue
		}
		wg.Add(1)
		go under(l, func(l *deltaLayer) error { return fetchLayer(ctx, b, src, ranges, l) })
	}
	// The ranged layers take their turns under the same limiter, one at a
	// time on this goroutine.
	for _, l := range layers {
		if l.reason != fallbackNone {
			continue
		}
		if err := withSlot(ctx, slots, func() error { return fetchLayer(ctx, b, src, ranges, l) }); err != nil {
			fail(err)
			break
		}
	}
	if err := wait(); err != nil {
		return Built{}, err
	}

	var size, tocBytes, planned, requests, fetched int64
	for _, l := range layers {
		slog.Info("delta: layer", "layer", l.desc.Digest, "size", l.desc.Size,
			"toc_bytes", l.tocBytes, "toc_ms", l.tocLatency.Milliseconds(), "entries", len(l.entries()),
			"missing", len(l.files), "planned", l.planned, "requests", len(l.requests),
			"fetched", l.fetched, "fallback", l.reason)
		size += l.desc.Size
		tocBytes += l.tocBytes
		planned += l.planned
		requests += int64(len(l.requests))
		fetched += l.fetched
	}
	slog.Info("delta: pull", "layers", len(layers), "size", size, "toc_bytes", tocBytes,
		"missing", len(missing), "planned", planned, "requests", requests, "fetched", fetched)

	return finishImage(ctx, store, img, root, res, b)
}

// withSlot runs fn once a slot is free and gives the slot back when fn
// returns; a pull cancelled while waiting for one returns the cancellation
// instead of running fn.
func withSlot(ctx context.Context, slots chan struct{}, fn func() error) error {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slots }()
	return fn()
}

// entries returns the layer's table of contents entries, none for a layer
// without a table.
func (l *deltaLayer) entries() []zstdchunked.Entry {
	if l.toc == nil {
		return nil
	}
	return l.toc.Entries
}

// readLayer reads l's table of contents and, when that leaves the layer to
// its tar stream, opens and scans the stream right away, every file stored
// on the way.
func readLayer(ctx context.Context, b *rootfs.Builder, src ocichain.BlobSource, ranges ocichain.RangeSource, store *objectstore.Store, res ocichain.Resolved, l *deltaLayer) error {
	readTOC(ctx, ranges, store, l)
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.toc != nil {
		return nil
	}
	stream, err := ocichain.OpenLayer(ctx, src, l.desc)
	if err != nil {
		return fmt.Errorf("materialize: open layer %d: %w", l.idx, err)
	}
	layer, err := b.Scan(ctx, stream, res.DiffIDs[l.idx], l.idx)
	if cerr := stream.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("materialize: layer %d: close: %w", l.idx, cerr))
	}
	if err != nil {
		return err
	}
	l.scanned = layer
	l.fetched += l.desc.Size
	return nil
}

// readTOC fetches and authenticates l's table of contents. Any failure to
// get one turns the layer over to the tar stream: that path verifies the
// whole blob against its descriptor, so a table that cannot be trusted
// costs bandwidth, never integrity. A pull the caller cancelled meanwhile is
// left for the caller to report.
func readTOC(ctx context.Context, ranges ocichain.RangeSource, store *objectstore.Store, l *deltaLayer) {
	start := time.Now()
	toc, spool, n, err := fetchTOC(ctx, ranges, store, l.desc)
	l.tocLatency = time.Since(start)
	// The bytes a failed attempt read are still bytes the pull fetched.
	l.fetched = n
	if ctx.Err() != nil {
		if spool != nil {
			removeSpool(spool)
		}
		return
	}
	if err != nil {
		slog.Warn("delta: falling back to whole layer", "layer", l.desc.Digest, "err", err)
		l.reason = fallbackTOCError
		return
	}
	if toc == nil {
		l.reason = fallbackNoTOC
		return
	}
	l.toc, l.spool, l.tocBytes, l.reason = toc, spool, n, fallbackNone
	if spool != nil {
		l.reason = fallbackWhole
	}
}

// fetchTOC reads and authenticates the table of contents of desc, reporting
// the bytes read for it whether or not it succeeded. A layer without a table
// yields a nil table. When the source answers the range request with the
// whole blob, the verified blob is spooled to a store temp and, when its
// table is accepted, returned open; the spool is then the caller's to remove.
func fetchTOC(ctx context.Context, ranges ocichain.RangeSource, store *objectstore.Store, desc ocispec.Descriptor) (*zstdchunked.TOC, *os.File, int64, error) {
	pos, ok, err := zstdchunked.Position(desc.Annotations, desc.Size)
	if err != nil || !ok {
		return nil, nil, 0, err
	}
	r, whole, err := ranges.OpenRange(ctx, desc, pos.Offset, pos.CompressedLen)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("materialize: fetch table of contents: %w", err)
	}
	defer warnClose("materialize: failed to close table of contents stream", r)
	counted := &countingReader{r: r}
	if whole {
		spool, err := spoolBlob(store, counted)
		if err != nil {
			return nil, nil, counted.n, err
		}
		data := make([]byte, pos.CompressedLen)
		if _, err := spool.ReadAt(data, pos.Offset); err != nil {
			removeSpool(spool)
			return nil, nil, counted.n, fmt.Errorf("materialize: read spooled table of contents: %w", err)
		}
		toc, err := zstdchunked.Parse(data, pos)
		if err != nil {
			removeSpool(spool)
			return nil, nil, counted.n, err
		}
		return toc, spool, counted.n, nil
	}
	// One byte past the length tells an over-long answer from an exact one;
	// Parse checks the checksum and the exact length.
	data, err := io.ReadAll(io.LimitReader(counted, pos.CompressedLen+1))
	if err != nil {
		return nil, nil, counted.n, fmt.Errorf("materialize: read table of contents: %w", err)
	}
	if int64(len(data)) != pos.CompressedLen {
		return nil, nil, counted.n, fmt.Errorf("materialize: table of contents range returned %d bytes, want %d", len(data), pos.CompressedLen)
	}
	toc, err := zstdchunked.Parse(data, pos)
	if err != nil {
		return nil, nil, counted.n, err
	}
	return toc, nil, counted.n, nil
}

// spoolBlob copies a whole blob stream into a store temp, reading it through
// so the stream's end-of-blob verification is observed, and returns the temp
// open.
func spoolBlob(store *objectstore.Store, r io.Reader) (*os.File, error) {
	f, err := os.CreateTemp(store.TempDir(), "delta-*") // #nosec G304 -- store-owned temp dir
	if err != nil {
		return nil, fmt.Errorf("materialize: create spool: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		removeSpool(f)
		return nil, fmt.Errorf("materialize: spool layer blob: %w", err)
	}
	return f, nil
}

// removeSpool closes and deletes a spool.
func removeSpool(f *os.File) {
	if err := f.Close(); err != nil {
		slog.Warn("materialize: failed to close spool", "path", f.Name(), "err", err)
	}
	if err := os.Remove(f.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("materialize: failed to remove spool", "path", f.Name(), "err", err)
	}
}

func removeSpools(layers []*deltaLayer) {
	for _, l := range layers {
		if l.spool != nil {
			removeSpool(l.spool)
		}
	}
}

// mergeLayer merges l into the build: from its table of contents, with the
// files pending, or as scanned from its tar stream.
func mergeLayer(b *rootfs.Builder, l *deltaLayer) error {
	if l.toc == nil {
		return b.Merge(l.scanned)
	}
	layer, err := rootfs.LayerFromTOC(l.toc, l.idx)
	if err != nil {
		return err
	}
	return b.Merge(layer)
}

// checkSpace refuses a pull whose missing payloads cannot fit in the store's
// filesystem. Their sizes are a lower bound on what the pull writes: the
// fs-verity trees and the composefs image come on top.
func checkSpace(store *objectstore.Store, missing []rootfs.Payload) error {
	var need int64
	for _, p := range missing {
		// The sizes come from the tables; a sum that cannot be counted cannot
		// be stored either.
		if p.Size > math.MaxInt64-need {
			return fmt.Errorf("%w: the missing content adds up to more than %d bytes", ErrNoSpace, math.MaxInt64)
		}
		need += p.Size
	}
	var st unix.Statfs_t
	if err := unix.Statfs(store.ObjectsDir(), &st); err != nil {
		return fmt.Errorf("materialize: statfs store: %w", err)
	}
	avail := int64(st.Bavail) * int64(st.Bsize) // #nosec G115 -- block counts fit in int64
	if need > avail {
		return fmt.Errorf("%w: the missing content needs at least %d bytes and %d are available (fs-verity metadata and the composefs image come on top)", ErrNoSpace, need, avail)
	}
	return nil
}

// plan assigns every missing payload to the lowest layer whose table lists
// it, at that layer's lowest-offset occurrence, so a payload is fetched once,
// and turns each layer's files into range requests: the extents in offset
// order, coalesced across a gap of at most coalesceGap. A layer that would
// take more than maxDeltaRequests requests, or whose requests add up to more
// than maxDeltaFraction of its blob, is read whole instead.
func plan(layers []*deltaLayer, missing []rootfs.Payload) error {
	type occurrence struct {
		layer *deltaLayer
		entry zstdchunked.Entry
	}
	sources := make(map[rootfs.Payload]occurrence)
	for _, l := range layers {
		for _, e := range l.entries() {
			if e.Digest == "" {
				continue
			}
			key := rootfs.Payload{Digest: e.Digest, Size: e.Size}
			if o, ok := sources[key]; !ok || (o.layer == l && e.Offset < o.entry.Offset) {
				sources[key] = occurrence{layer: l, entry: e}
			}
		}
	}
	for _, p := range missing {
		o, ok := sources[p]
		if !ok {
			return fmt.Errorf("materialize: no table of contents lists missing payload %s of %d bytes", p.Digest, p.Size)
		}
		o.layer.files = append(o.layer.files, fileFetch{payload: p, entry: o.entry})
	}

	for _, l := range layers {
		sort.Slice(l.files, func(i, j int) bool { return l.files[i].entry.Offset < l.files[j].entry.Offset })
		for _, f := range l.files {
			if n := len(l.requests); n > 0 && f.entry.Offset-l.requests[n-1].end <= coalesceGap {
				l.requests[n-1].end = f.entry.EndOffset
				l.requests[n-1].files = append(l.requests[n-1].files, f)
				continue
			}
			l.requests = append(l.requests, byteRange{off: f.entry.Offset, end: f.entry.EndOffset, files: []fileFetch{f}})
		}
		for _, r := range l.requests {
			l.planned += r.end - r.off
		}
		if l.reason != fallbackNone {
			continue
		}
		switch {
		case len(l.requests) > maxDeltaRequests:
			l.reason = fallbackRequests
		case float64(l.planned) > maxDeltaFraction*float64(l.desc.Size):
			l.reason = fallbackFraction
		}
	}
	return nil
}

// fetchLayer supplies the builder with l's missing files: out of the spool,
// out of the whole blob read once in offset order, or range by range, each
// range consumed in offset order. A range the source answers with the whole
// blob serves that request and every later one of the layer from that
// stream, and no further range is requested.
func fetchLayer(ctx context.Context, b *rootfs.Builder, src ocichain.BlobSource, ranges ocichain.RangeSource, l *deltaLayer) error {
	if len(l.files) == 0 {
		return nil
	}
	if l.spool != nil {
		for _, f := range l.files {
			if err := supplyFile(ctx, b, f, io.NewSectionReader(l.spool, f.entry.Offset, f.entry.EndOffset-f.entry.Offset)); err != nil {
				return err
			}
		}
		return nil
	}
	if l.reason != fallbackNone {
		stream, err := ocichain.OpenBlob(ctx, src, l.desc)
		if err != nil {
			return fmt.Errorf("materialize: open layer %d: %w", l.idx, err)
		}
		return supplyClosing(ctx, b, l, stream, 0, l.files, true)
	}
	for i, r := range l.requests {
		stream, whole, err := ranges.OpenRange(ctx, l.desc, r.off, r.end-r.off)
		if err != nil {
			return fmt.Errorf("materialize: layer %d: fetch range %d+%d: %w", l.idx, r.off, r.end-r.off, err)
		}
		if whole {
			l.reason = fallbackWhole
			var rest []fileFetch
			for _, r := range l.requests[i:] {
				rest = append(rest, r.files...)
			}
			return supplyClosing(ctx, b, l, stream, 0, rest, true)
		}
		if err := supplyClosing(ctx, b, l, stream, r.off, r.files, false); err != nil {
			return err
		}
	}
	return nil
}

// supplyClosing supplies files out of stream, which starts at blob offset
// base, counts the bytes read into l, and closes the stream. A whole blob is
// verified against its descriptor at end of stream, so drain reads such a
// stream through and reports that verdict.
func supplyClosing(ctx context.Context, b *rootfs.Builder, l *deltaLayer, stream io.ReadCloser, base int64, files []fileFetch, drain bool) error {
	counted := &countingReader{r: stream}
	err := supplyStream(ctx, b, counted, base, files, drain)
	l.fetched += counted.n
	if cerr := stream.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("materialize: layer %d: close: %w", l.idx, cerr))
	}
	return err
}

// supplyStream reads files, in offset order, out of a stream of the blob
// that starts at offset base, skipping the bytes between them.
func supplyStream(ctx context.Context, b *rootfs.Builder, r io.Reader, base int64, files []fileFetch, drain bool) error {
	pos := base
	for _, f := range files {
		skip := f.entry.Offset - pos
		if skip < 0 {
			return fmt.Errorf("materialize: file %s at offset %d is behind the stream at %d", f.entry.Name, f.entry.Offset, pos)
		}
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			return fmt.Errorf("materialize: skip to %s: %w", f.entry.Name, err)
		}
		extent := io.LimitReader(r, f.entry.EndOffset-f.entry.Offset)
		if err := supplyFile(ctx, b, f, extent); err != nil {
			return err
		}
		// The decoder need not have consumed the extent to its last byte.
		if _, err := io.Copy(io.Discard, extent); err != nil {
			return fmt.Errorf("materialize: skip past %s: %w", f.entry.Name, err)
		}
		pos = f.entry.EndOffset
	}
	if drain {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return fmt.Errorf("materialize: verify layer blob: %w", err)
		}
	}
	return nil
}

// supplyFile reconstructs one file from the compressed bytes of its extent
// and supplies it to the builder, which checks it against its digest.
func supplyFile(ctx context.Context, b *rootfs.Builder, f fileFetch, compressed io.Reader) error {
	r, err := zstdchunked.Reconstruct(f.entry, compressed)
	if err != nil {
		return err
	}
	err = b.Supply(ctx, f.payload, r)
	if cerr := r.Close(); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if err != nil {
		return fmt.Errorf("materialize: supply %s: %w", f.entry.Name, err)
	}
	return nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func warnClose(msg string, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(msg, "err", err)
	}
}
