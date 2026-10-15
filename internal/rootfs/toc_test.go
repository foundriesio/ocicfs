// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/zstdchunked"
)

// TestLayerFromTOCMatchesScan asserts that, for every zstd:chunked fixture
// layer, the table of contents yields the records a scan of the layer's tar
// stream yields: the same kinds, names, targets and node metadata in the same
// order, with each regular file's pending payload naming the digest and size
// of the bytes the scan captured.
func TestLayerFromTOCMatchesScan(t *testing.T) {
	for _, layout := range fixtureLayouts {
		for _, ref := range fixtureRefs(t, layout) {
			img := loadFixtureImage(t, layout, ref)
			for i := range img.layers {
				toc, ok := img.toc(t, i)
				if !ok {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/layer%d", filepath.Base(layout), ref, i), func(t *testing.T) {
					scanned, err := scanLayer(context.Background(), digestSink{}, img.open(t, i), img.diffIDs[i], i)
					if err != nil {
						t.Fatalf("scanLayer: %v", err)
					}
					fromTOC, err := LayerFromTOC(toc, i)
					if err != nil {
						t.Fatalf("LayerFromTOC: %v", err)
					}
					assertSameRecords(t, fromTOC, scanned)
				})
			}
		}
	}
}

// TestLayerFromTOCClampsMtime asserts a table-of-contents entry without a
// modification time, or with one before the epoch, gets the clamp a tar
// header gets.
func TestLayerFromTOCClampsMtime(t *testing.T) {
	preEpoch := time.Unix(-5, 0)
	subSecond := time.Unix(1700000000, 123456789)
	toc := &zstdchunked.TOC{Entries: []zstdchunked.Entry{
		{Type: zstdchunked.TypeDir, Name: "none", Mode: 0o755},
		{Type: zstdchunked.TypeDir, Name: "pre-epoch", Mode: 0o755, ModTime: &preEpoch},
		{Type: zstdchunked.TypeDir, Name: "sub-second", Mode: 0o755, ModTime: &subSecond},
	}}
	l, err := LayerFromTOC(toc, 0)
	if err != nil {
		t.Fatalf("LayerFromTOC: %v", err)
	}
	want := [][2]int64{{0, 0}, {0, 0}, {1700000000, 123456789}}
	for i, rec := range l.records {
		if got := [2]int64{rec.n.mtimeSec, rec.n.mtimeNsec}; got != want[i] {
			t.Errorf("%s mtime = %v, want %v", rec.name, got, want[i])
		}
	}
}

// TestLayerFromTOCRejects asserts the checks a scan makes on a tar entry are
// made on a table-of-contents entry too, and that a regular file with bytes
// but no digest to fetch them by is refused.
func TestLayerFromTOCRejects(t *testing.T) {
	cases := map[string]zstdchunked.Entry{
		"escaping name":       {Type: zstdchunked.TypeDir, Name: "../etc"},
		"escaping link":       {Type: zstdchunked.TypeHardlink, Name: "a", Linkname: "../../b"},
		"empty symlink":       {Type: zstdchunked.TypeSymlink, Name: "a"},
		"empty whiteout":      {Type: zstdchunked.TypeReg, Name: "dir/.wh."},
		"unknown type":        {Type: "socket", Name: "a"},
		"file with no digest": {Type: zstdchunked.TypeReg, Name: "a", Size: 1},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LayerFromTOC(&zstdchunked.TOC{Entries: []zstdchunked.Entry{e}}, 0); err == nil {
				t.Fatal("LayerFromTOC accepted the entry")
			}
		})
	}
}

// TestLayerFromTOCHardlinkWithSize asserts an entry is dispatched on its type:
// a hardlink that repeats its target's size and digest is still a hardlink.
func TestLayerFromTOCHardlinkWithSize(t *testing.T) {
	d := digest.FromString("target")
	toc := &zstdchunked.TOC{Entries: []zstdchunked.Entry{
		{Type: zstdchunked.TypeReg, Name: "target", Size: 100, Digest: d},
		{Type: zstdchunked.TypeHardlink, Name: "link", Linkname: "./target", Size: 100, Digest: d},
	}}
	l, err := LayerFromTOC(toc, 0)
	if err != nil {
		t.Fatalf("LayerFromTOC: %v", err)
	}
	link := l.records[1]
	if link.kind != kindHardlink || link.target != "target" || link.n != nil {
		t.Fatalf("hardlink record = %+v", link)
	}
}

// TestTOCPathMatchesTarPath asserts that, for every fixture image, building
// from the tables of contents and fetching only what the merged tree lacks
// gives the image the tar walk gives: the same descriptor, fs-verity digest,
// canonical flag and entries, with every object the image references present
// in the store and nothing else fetched. A layer without a table of contents
// is scanned on both paths. Each path gets a store of its own, so neither
// finds the other's objects.
func TestTOCPathMatchesTarPath(t *testing.T) {
	ctx := context.Background()
	for _, layout := range fixtureLayouts {
		for _, ref := range fixtureRefs(t, layout) {
			t.Run(filepath.Base(layout)+"/"+ref, func(t *testing.T) {
				img := loadFixtureImage(t, layout, ref)

				tarStore := newVerityStore(t)
				tarBuilder := NewBuilder(tarStore)
				defer tarBuilder.Close()
				want, wantErr := buildFromTar(t, tarBuilder, img)
				skipIfNoVerity(t, wantErr)

				tocStore := newVerityStore(t)
				tocBuilder := NewBuilder(tocStore)
				defer tocBuilder.Close()
				got, _, err := buildFromTOC(t, tocBuilder, img)
				skipIfNoVerity(t, err)

				// A layer set the tar walk refuses is refused alike.
				if wantErr != nil || err != nil {
					if wantErr == nil || err == nil || err.Error() != wantErr.Error() {
						t.Fatalf("TOC path error = %v, want %v", err, wantErr)
					}
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("TOC path image = %+v, want %+v", got, want)
				}
				// The merged tree outlives Finish, and deriving its entries again
				// gives the ones Finish wrote.
				gotEntries, _, err := tocBuilder.m.entries()
				if err != nil {
					t.Fatalf("TOC path entries: %v", err)
				}
				wantEntries, _, err := tarBuilder.m.entries()
				if err != nil {
					t.Fatalf("tar path entries: %v", err)
				}
				if !reflect.DeepEqual(gotEntries, wantEntries) {
					t.Errorf("TOC path entries = %+v, want %+v", gotEntries, wantEntries)
				}

				wantRefs := referencedObjects(t, tarStore, want)
				gotRefs := referencedObjects(t, tocStore, got)
				if !reflect.DeepEqual(gotRefs, wantRefs) {
					t.Errorf("TOC path references %v, want %v", gotRefs, wantRefs)
				}
				// The TOC path holds the image and what it references. With no
				// scanned layer it holds nothing else: a payload the merge dropped
				// was never fetched.
				stored := 0
				if err := tocStore.Walk(ctx, func(digest.Digest, int64) error { stored++; return nil }); err != nil {
					t.Fatalf("Walk: %v", err)
				}
				if ref != "mixed" && stored != len(gotRefs)+1 {
					t.Errorf("TOC path stored %d objects, want the image and its %d references", stored, len(gotRefs))
				}
			})
		}
	}
}

// TestTOCPathReusesStoredPayloads builds two releases of one image into one
// store through their tables of contents: the second build is asked only for
// the payloads the first did not leave behind, and still produces the image
// the tar walk produces.
func TestTOCPathReusesStoredPayloads(t *testing.T) {
	first := loadFixtureImage(t, fixtureLayouts[1], "n")
	second := loadFixtureImage(t, fixtureLayouts[2], "n1")

	tarBuilder := NewBuilder(newVerityStore(t))
	defer tarBuilder.Close()
	want, err := buildFromTar(t, tarBuilder, second)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("tar path: %v", err)
	}

	coldBuilder := NewBuilder(newVerityStore(t))
	defer coldBuilder.Close()
	_, cold, err := buildFromTOC(t, coldBuilder, second)
	if err != nil {
		t.Fatalf("TOC path into an empty store: %v", err)
	}

	store := newVerityStore(t)
	firstBuilder := NewBuilder(store)
	defer firstBuilder.Close()
	if _, _, err := buildFromTOC(t, firstBuilder, first); err != nil {
		t.Fatalf("TOC path, first release: %v", err)
	}
	secondBuilder := NewBuilder(store)
	defer secondBuilder.Close()
	got, warm, err := buildFromTOC(t, secondBuilder, second)
	if err != nil {
		t.Fatalf("TOC path, second release: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("second release image = %+v, want %+v", got, want)
	}
	referencedObjects(t, store, got)

	stored := func(payloads []Payload) int {
		n := 0
		for _, p := range payloads {
			if p.Size > composefs.InlineContentMax {
				n++
			}
		}
		return n
	}
	if stored(warm) >= stored(cold) {
		t.Errorf("second release asked for %d stored payloads after the first release, %d into an empty store", stored(warm), stored(cold))
	}
	if len(warm)-stored(warm) != len(cold)-stored(cold) {
		t.Errorf("second release asked for %d inline payloads, want all %d", len(warm)-stored(warm), len(cold)-stored(cold))
	}
}

// TestSupplyConcurrently asserts payloads supplied from several goroutines at
// once build the image a sequential supply builds.
func TestSupplyConcurrently(t *testing.T) {
	img := loadFixtureImage(t, fixtureLayouts[2], "n1")
	ctx := context.Background()

	seqBuilder := NewBuilder(newVerityStore(t))
	defer seqBuilder.Close()
	want, _, err := buildFromTOC(t, seqBuilder, img)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("sequential supply: %v", err)
	}

	store := newVerityStore(t)
	b := NewBuilder(store)
	defer b.Close()
	sources, err := mergeFromTOC(t, b, img)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	missing, err := b.Missing(ctx)
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if len(missing) < 2 {
		t.Fatalf("Missing returned %d payloads, want at least 2 to supply concurrently", len(missing))
	}
	errs := make(chan error, len(missing))
	for _, p := range missing {
		go func() { errs <- supplyFromBlob(b, sources, p) }()
	}
	// Every supplier is joined before the builder can be closed on a failure.
	var failed error
	for range missing {
		if err := <-errs; err != nil && failed == nil {
			failed = err
		}
	}
	if failed != nil {
		t.Fatalf("concurrent Supply: %v", failed)
	}
	got, err := b.Finish(ctx)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("image = %+v, want %+v", got, want)
	}
	referencedObjects(t, store, got)
}

// TestMissingSkipsDroppedPayloads asserts a payload the merge removed is not
// asked for: the upper layer of the fixture whites out the lower layer's only
// stored file that is not also kept under another name.
func TestMissingSkipsDroppedPayloads(t *testing.T) {
	img := loadFixtureImage(t, fixtureLayouts[0], "dir-replaced-by-file")
	b := NewBuilder(newVerityStore(t))
	defer b.Close()
	listed := make(map[digest.Digest]bool)
	for i := range img.layers {
		toc, _ := img.toc(t, i)
		for _, e := range toc.Entries {
			if e.Digest != "" {
				listed[e.Digest] = true
			}
		}
		l, err := LayerFromTOC(toc, i)
		if err != nil {
			t.Fatalf("LayerFromTOC: %v", err)
		}
		if err := b.Merge(l); err != nil {
			t.Fatalf("Merge: %v", err)
		}
	}
	missing, err := b.Missing(context.Background())
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if len(missing) >= len(listed) {
		t.Fatalf("Missing returned %d payloads of the %d the layers list; the merge dropped none", len(missing), len(listed))
	}
}

// TestMissingResolvesPresentObject asserts a stored payload the store already
// holds is resolved in place — not returned, its view link repaired when a
// crash left it missing — and that the image references it.
func TestMissingResolvesPresentObject(t *testing.T) {
	ctx := context.Background()
	store := newVerityStore(t)
	body := large("present")
	d, sealed, err := store.WriteObject(ctx, strings.NewReader(body))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	fsv, err := sealed()
	if err != nil {
		t.Fatalf("Sealed: %v", err)
	}
	viewPath := filepath.Join(store.ViewDir(), fsv[len("sha256:"):][:2], fsv[len("sha256:"):][2:])
	if err := os.Remove(viewPath); err != nil {
		t.Fatalf("remove view link: %v", err)
	}

	b := NewBuilder(store)
	defer b.Close()
	mergeTOC(t, b, zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "present", Mode: 0o644, Size: int64(len(body)), Digest: d})
	missing, err := b.Missing(ctx)
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("Missing = %v, want nothing", missing)
	}
	if _, err := os.Lstat(viewPath); err != nil {
		t.Fatalf("view link not repaired: %v", err)
	}
	img, err := b.Finish(ctx)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if refs := referencedObjects(t, store, img); len(refs) != 1 || refs[0] != fsv[len("sha256:"):][:2]+"/"+fsv[len("sha256:"):][2:] {
		t.Fatalf("image references %v, want the present object %s", refs, fsv)
	}
}

// TestSupplyInlinesPayloadHeldAsObject asserts an inline-sized payload whose
// bytes the store happens to hold as an object is still asked for and inlined,
// as the tar walk would inline it, rather than referenced by digest.
func TestSupplyInlinesPayloadHeldAsObject(t *testing.T) {
	ctx := context.Background()
	store := newVerityStore(t)
	const body = "small enough to inline"
	d, _, err := store.WriteObject(ctx, strings.NewReader(body))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}

	b := NewBuilder(store)
	defer b.Close()
	mergeTOC(t, b,
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "small", Mode: 0o644, Size: int64(len(body)), Digest: d},
		zstdchunked.Entry{Type: zstdchunked.TypeHardlink, Name: "small-link", Linkname: "small"},
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "small-copy", Mode: 0o600, Size: int64(len(body)), Digest: d},
	)
	missing, err := b.Missing(ctx)
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	want := []Payload{{Digest: d, Size: int64(len(body))}}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("Missing = %v, want %v", missing, want)
	}
	if err := b.Supply(ctx, missing[0], strings.NewReader(body)); err != nil {
		t.Fatalf("Supply: %v", err)
	}
	if _, err := b.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	entries, _, err := b.m.entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	byPath := entriesByPath(entries)
	for _, p := range []string{"/small", "/small-copy"} {
		if e := byPath[p]; string(e.Content) != body || e.Digest != "" || e.Verity != "" {
			t.Errorf("%s = {content %q digest %q verity %q}, want the bytes inline", p, e.Content, e.Digest, e.Verity)
		}
	}
	if e := byPath["/small-link"]; e.Hardlink != "/small" {
		t.Errorf("/small-link hardlink = %q, want /small", e.Hardlink)
	}
}

// TestSupplyRejects asserts Supply takes only a payload Missing returned, once,
// and only bytes that are the payload; a rejected payload stays missing and
// can be supplied again.
func TestSupplyRejects(t *testing.T) {
	ctx := context.Background()
	store := newVerityStore(t)
	const small = "inline payload"
	big := large("stored payload")
	smallP := Payload{Digest: digest.FromString(small), Size: int64(len(small))}
	bigP := Payload{Digest: digest.FromString(big), Size: int64(len(big))}

	b := NewBuilder(store)
	defer b.Close()
	mergeTOC(t, b,
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "big", Mode: 0o644, Size: bigP.Size, Digest: bigP.Digest},
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "small", Mode: 0o644, Size: smallP.Size, Digest: smallP.Digest},
	)
	if err := b.Supply(ctx, smallP, strings.NewReader(small)); err == nil {
		t.Fatal("Supply before Missing succeeded")
	}
	missing, err := b.Missing(ctx)
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if want := []Payload{bigP, smallP}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("Missing = %v, want %v", missing, want)
	}
	if _, err := b.Missing(ctx); err == nil {
		t.Fatal("second Missing succeeded")
	}

	rejects := []struct {
		name string
		p    Payload
		body string
	}{
		{"inline short", smallP, small[:len(small)-1]},
		{"inline long", smallP, small + "x"},
		{"inline other bytes", smallP, strings.Repeat("x", len(small))},
		{"stored other bytes", bigP, strings.Repeat("x", len(big))},
		{"unknown digest", Payload{Digest: digest.FromString("unknown"), Size: 5}, "12345"},
		{"other size", Payload{Digest: smallP.Digest, Size: smallP.Size + 1}, small},
	}
	for _, r := range rejects {
		if err := b.Supply(ctx, r.p, strings.NewReader(r.body)); err == nil {
			t.Errorf("Supply(%s) succeeded", r.name)
		}
	}
	assertNoTemps(t, store)

	if err := b.Supply(ctx, smallP, strings.NewReader(small)); err != nil {
		t.Fatalf("Supply(small): %v", err)
	}
	if err := b.Supply(ctx, smallP, strings.NewReader(small)); err == nil {
		t.Fatal("second Supply of one payload succeeded")
	}
	err = b.Supply(ctx, bigP, strings.NewReader(big))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Supply(big): %v", err)
	}
	if _, err := b.Finish(ctx); err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("Finish: %v", err)
	}
	if err := b.Supply(ctx, bigP, strings.NewReader(big)); !errors.Is(err, errBuilderSpent) {
		t.Fatalf("Supply after Finish = %v, want errBuilderSpent", err)
	}
	if _, err := b.Missing(ctx); !errors.Is(err, errBuilderSpent) {
		t.Fatalf("Missing after Finish = %v, want errBuilderSpent", err)
	}
}

// TestPayloadSizeMustBeTheObjects asserts a stored payload listed with a size
// other than its bytes' is refused whether the store already holds the object
// or the bytes are supplied: that size would go into the image.
func TestPayloadSizeMustBeTheObjects(t *testing.T) {
	ctx := context.Background()
	body := large("sized")
	d := digest.FromString(body)
	listed := zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "f", Mode: 0o644, Size: int64(len(body)) + 1, Digest: d}

	t.Run("object present", func(t *testing.T) {
		store := newVerityStore(t)
		_, _, err := store.WriteObject(ctx, strings.NewReader(body))
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("WriteObject: %v", err)
		}
		b := NewBuilder(store)
		defer b.Close()
		mergeTOC(t, b, listed)
		if _, err := b.Missing(ctx); err == nil {
			t.Fatal("Missing accepted a size that is not the stored object's")
		}
	})

	t.Run("bytes supplied", func(t *testing.T) {
		store := newVerityStore(t)
		b := NewBuilder(store)
		defer b.Close()
		mergeTOC(t, b, listed)
		missing, err := b.Missing(ctx)
		if err != nil || len(missing) != 1 {
			t.Fatalf("Missing = %v, %v, want one payload", missing, err)
		}
		if err := b.Supply(ctx, missing[0], strings.NewReader(body)); err == nil {
			t.Fatal("Supply accepted bytes of another size than listed")
		}
		if _, err := b.Finish(ctx); err == nil {
			t.Fatal("Finish succeeded with the payload refused")
		}
	})
}

// TestSupplyObservesItsContext asserts a stored payload stops being staged
// once the context of its Supply is cancelled, though the writer's context,
// which Missing was given, is still live.
func TestSupplyObservesItsContext(t *testing.T) {
	store := newVerityStore(t)
	body := large("cancelled")
	b := NewBuilder(store)
	defer b.Close()
	mergeTOC(t, b, zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "f", Mode: 0o644, Size: int64(len(body)), Digest: digest.FromString(body)})
	missing, err := b.Missing(context.Background())
	if err != nil || len(missing) != 1 {
		t.Fatalf("Missing = %v, %v, want one payload", missing, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Supply(ctx, missing[0], strings.NewReader(body)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supply = %v, want context.Canceled", err)
	}
	assertNoTemps(t, store)
}

// TestMergeAfterMissingIsRefused asserts the tree cannot change once Missing
// has listed its payloads.
func TestMergeAfterMissingIsRefused(t *testing.T) {
	b := NewBuilder(newVerityStore(t))
	defer b.Close()
	mergeTOC(t, b, zstdchunked.Entry{Type: zstdchunked.TypeDir, Name: "usr/", Mode: 0o755})
	if _, err := b.Missing(context.Background()); err != nil {
		t.Fatalf("Missing: %v", err)
	}
	l, err := LayerFromTOC(&zstdchunked.TOC{}, 1)
	if err != nil {
		t.Fatalf("LayerFromTOC: %v", err)
	}
	if err := b.Merge(l); err == nil {
		t.Fatal("Merge after Missing succeeded")
	}
}

// TestFinishNamesUnsuppliedPayload asserts Finish refuses a tree with a
// payload nobody supplied, names it, and leaves no writer or temp behind —
// whether Missing was called or not.
func TestFinishNamesUnsuppliedPayload(t *testing.T) {
	ctx := context.Background()
	body := large("never supplied")
	d := digest.FromString(body)
	for _, callMissing := range []bool{true, false} {
		store := newVerityStore(t)
		b := NewBuilder(store)
		mergeTOC(t, b, zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "f", Mode: 0o644, Size: int64(len(body)), Digest: d})
		if callMissing {
			if _, err := b.Missing(ctx); err != nil {
				t.Fatalf("Missing: %v", err)
			}
		}
		_, err := b.Finish(ctx)
		if err == nil || !strings.Contains(err.Error(), d.String()) {
			t.Fatalf("Finish (Missing called: %v) = %v, want an error naming %s", callMissing, err, d)
		}
		if b.w != nil {
			t.Fatal("Finish left the writer open")
		}
		assertNoTemps(t, store)
	}
}

// TestCloseAfterMissingLeavesNothing asserts abandoning a build after Missing
// — with a payload queued, and after a Supply that failed — stops the writer
// and leaves no staged temp. Close returns only once the writer's worker has
// exited, so returning at all shows no goroutine is left.
func TestCloseAfterMissingLeavesNothing(t *testing.T) {
	ctx := context.Background()
	store := newVerityStore(t)
	kept, broken := large("kept"), large("broken")
	b := NewBuilder(store)
	mergeTOC(t, b,
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "kept", Mode: 0o644, Size: int64(len(kept)), Digest: digest.FromString(kept)},
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "broken", Mode: 0o644, Size: int64(len(broken)), Digest: digest.FromString(broken)},
	)
	if _, err := b.Missing(ctx); err != nil {
		t.Fatalf("Missing: %v", err)
	}
	err := b.Supply(ctx, Payload{Digest: digest.FromString(kept), Size: int64(len(kept))}, strings.NewReader(kept))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Supply: %v", err)
	}
	if err := b.Supply(ctx, Payload{Digest: digest.FromString(broken), Size: int64(len(broken))}, iotest.ErrReader(errors.New("range read failed"))); err == nil {
		t.Fatal("Supply from a failing reader succeeded")
	}
	b.Close()
	if b.w != nil {
		t.Fatal("Close left the writer open")
	}
	assertNoTemps(t, store)
	b.Close()

	empty := NewBuilder(store)
	if _, err := empty.Missing(ctx); err != nil {
		t.Fatalf("Missing on an empty tree: %v", err)
	}
	empty.Close()
	if _, err := empty.Finish(ctx); !errors.Is(err, errBuilderSpent) {
		t.Fatalf("Finish after Close = %v, want errBuilderSpent", err)
	}
}

// TestMissingRejectsOneDigestTwoSizes asserts a digest named with two sizes,
// which no content can satisfy, is refused rather than fetched as either.
func TestMissingRejectsOneDigestTwoSizes(t *testing.T) {
	d := digest.FromString("one digest")
	b := NewBuilder(newVerityStore(t))
	defer b.Close()
	mergeTOC(t, b,
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "a", Mode: 0o644, Size: 10, Digest: d},
		zstdchunked.Entry{Type: zstdchunked.TypeReg, Name: "b", Mode: 0o644, Size: 100, Digest: d},
	)
	if _, err := b.Missing(context.Background()); err == nil {
		t.Fatal("Missing accepted one digest with two sizes")
	}
}

// buildFromTar builds img by scanning every layer's tar stream.
func buildFromTar(t *testing.T, b *Builder, img fixtureImage) (Image, error) {
	t.Helper()
	ctx := context.Background()
	for i := range img.layers {
		if err := scanMerge(ctx, b, img.open(t, i), img.diffIDs[i]); err != nil {
			return Image{}, err
		}
	}
	return b.Finish(ctx)
}

// buildFromTOC builds img through the table-of-contents path: every layer
// that has a table becomes records without being read, the rest are scanned,
// and each payload Missing returns is reconstructed from the byte range of
// the layer blob its entry names. It also returns the payloads it supplied.
func buildFromTOC(t *testing.T, b *Builder, img fixtureImage) (Image, []Payload, error) {
	t.Helper()
	ctx := context.Background()
	sources, err := mergeFromTOC(t, b, img)
	if err != nil {
		return Image{}, nil, err
	}
	missing, err := b.Missing(ctx)
	if err != nil {
		return Image{}, nil, err
	}
	for _, p := range missing {
		if err := supplyFromBlob(b, sources, p); err != nil {
			return Image{}, nil, err
		}
	}
	built, err := b.Finish(ctx)
	return built, missing, err
}

// payloadSource is the layer blob and entry a table of contents lists a
// payload under.
type payloadSource struct {
	blob  []byte
	entry zstdchunked.Entry
}

// mergeFromTOC merges every layer of img into b, from its table when it has
// one and by scanning its tar stream otherwise, and returns where the tables
// list every payload.
func mergeFromTOC(t *testing.T, b *Builder, img fixtureImage) (map[digest.Digest]payloadSource, error) {
	t.Helper()
	ctx := context.Background()
	sources := make(map[digest.Digest]payloadSource)
	for i := range img.layers {
		toc, ok := img.toc(t, i)
		if !ok {
			if err := scanMerge(ctx, b, img.open(t, i), img.diffIDs[i]); err != nil {
				return nil, err
			}
			continue
		}
		for _, e := range toc.Entries {
			if e.Digest != "" {
				sources[e.Digest] = payloadSource{blob: img.blobs[i], entry: e}
			}
		}
		l, err := LayerFromTOC(toc, i)
		if err != nil {
			return nil, err
		}
		if err := b.Merge(l); err != nil {
			return nil, err
		}
	}
	return sources, nil
}

// supplyFromBlob supplies p to b, reconstructed from the byte range of the
// layer blob its entry names.
func supplyFromBlob(b *Builder, sources map[digest.Digest]payloadSource, p Payload) error {
	src, ok := sources[p.Digest]
	if !ok {
		return fmt.Errorf("Missing returned %s, which no table of contents lists", p.Digest)
	}
	r, err := zstdchunked.Reconstruct(src.entry, bytes.NewReader(src.blob[src.entry.Offset:src.entry.EndOffset]))
	if err != nil {
		return err
	}
	err = b.Supply(context.Background(), p, r)
	_ = r.Close()
	return err
}

// mergeTOC merges one hand-made table-of-contents layer into b.
func mergeTOC(t *testing.T, b *Builder, entries ...zstdchunked.Entry) {
	t.Helper()
	l, err := LayerFromTOC(&zstdchunked.TOC{Entries: entries}, 0)
	if err != nil {
		t.Fatalf("LayerFromTOC: %v", err)
	}
	if err := b.Merge(l); err != nil {
		t.Fatalf("Merge: %v", err)
	}
}

// referencedObjects returns the objects img references, after checking that
// store holds each of them under its fs-verity view name.
func referencedObjects(t *testing.T, store *objectstore.Store, img Image) []string {
	t.Helper()
	imagePath, err := store.ObjectPath(img.Descriptor.Digest)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	refs, err := composefs.ReferencedObjects(context.Background(), imagePath)
	if err != nil {
		t.Fatalf("ReferencedObjects: %v", err)
	}
	for _, name := range refs {
		if _, err := os.Lstat(filepath.Join(store.ViewDir(), name)); err != nil {
			t.Errorf("referenced object %s is not in the store: %v", name, err)
		}
	}
	return refs
}

// assertNoTemps fails the test if a staging temp is left in the store.
func assertNoTemps(t *testing.T, store *objectstore.Store) {
	t.Helper()
	entries, err := os.ReadDir(store.TempDir())
	if err != nil {
		t.Fatalf("ReadDir temp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir holds %d entries, want none", len(entries))
	}
}

// assertSameRecords fails the test unless the layer made from a table of
// contents matches the scanned one record for record.
func assertSameRecords(t *testing.T, fromTOC, scanned *Layer) {
	t.Helper()
	if len(fromTOC.records) != len(scanned.records) {
		t.Fatalf("records = %d, want %d", len(fromTOC.records), len(scanned.records))
	}
	for i, want := range scanned.records {
		got := fromTOC.records[i]
		if got.kind != want.kind || got.name != want.name || got.target != want.target {
			t.Fatalf("record %d = {%d %q %q}, want {%d %q %q}", i, got.kind, got.name, got.target, want.kind, want.name, want.target)
		}
		if (got.n == nil) != (want.n == nil) {
			t.Fatalf("record %d (%s): node presence differs", i, want.name)
		}
		if want.n == nil {
			continue
		}
		// The scan captured the bytes; the table of contents names them. Compare
		// the payloads through their digests, then everything else as is.
		g, w := *got.n, *want.n
		wantDigest, wantSize := w.digest, w.size
		if len(w.content) > 0 {
			wantDigest = digest.FromBytes(w.content)
		}
		if g.digest != wantDigest || g.size != wantSize || g.sealed != nil || g.verity != "" {
			t.Errorf("record %d (%s): payload = {%s %d}, want {%s %d}", i, want.name, g.digest, g.size, wantDigest, wantSize)
		}
		if wantDigest == "" && (g.content == nil) != (w.content == nil) {
			t.Errorf("record %d (%s): content = %v, want %v", i, want.name, g.content, w.content)
		}
		if wantDigest != "" && g.content != nil {
			t.Errorf("record %d (%s): pending payload carries content", i, want.name)
		}
		g.digest, g.size, g.content, w.digest, w.size, w.content, w.sealed = "", 0, nil, "", 0, nil, nil
		if !reflect.DeepEqual(g, w) {
			t.Errorf("record %d (%s): node = %+v, want %+v", i, want.name, g, w)
		}
	}
}

// digestSink reports each object's real content digest and discards its
// bytes, so a scan needs no store.
type digestSink struct{}

func (digestSink) WriteObject(_ context.Context, r io.Reader) (digest.Digest, objectstore.Sealed, error) {
	d, err := digest.FromReader(r)
	if err != nil {
		return "", nil, err
	}
	return d, objectstore.Resolved("sha256:" + strings.Repeat("0", 64)), nil
}

// fixtureLayouts are the zstd:chunked OCI layouts shared with the package
// that parses them.
var fixtureLayouts = []string{
	filepath.Join("..", "zstdchunked", "testdata", "layout"),
	filepath.Join("..", "zstdchunked", "testdata", "release-n"),
	filepath.Join("..", "zstdchunked", "testdata", "release-n1"),
}

// fixtureImage is one image of a fixture layout: its layer descriptors, their
// compressed blobs, and the diff_ids its config names.
type fixtureImage struct {
	layers  []ocispec.Descriptor
	blobs   [][]byte
	diffIDs []digest.Digest
}

// fixtureRefs lists the images a fixture layout holds.
func fixtureRefs(t *testing.T, layoutDir string) []string {
	t.Helper()
	var refs []string
	for _, desc := range readFixtureJSON[ocispec.Index](t, filepath.Join(layoutDir, "index.json")).Manifests {
		refs = append(refs, desc.Annotations[ocispec.AnnotationRefName])
	}
	return refs
}

func loadFixtureImage(t *testing.T, layoutDir, ref string) fixtureImage {
	t.Helper()
	blobPath := func(d digest.Digest) string {
		return filepath.Join(layoutDir, "blobs", d.Algorithm().String(), d.Encoded())
	}
	for _, desc := range readFixtureJSON[ocispec.Index](t, filepath.Join(layoutDir, "index.json")).Manifests {
		if desc.Annotations[ocispec.AnnotationRefName] != ref {
			continue
		}
		manifest := readFixtureJSON[ocispec.Manifest](t, blobPath(desc.Digest))
		config := readFixtureJSON[ocispec.Image](t, blobPath(manifest.Config.Digest))
		img := fixtureImage{layers: manifest.Layers, diffIDs: config.RootFS.DiffIDs}
		for _, layer := range manifest.Layers {
			blob, err := os.ReadFile(blobPath(layer.Digest))
			if err != nil {
				t.Fatalf("read layer %s: %v", layer.Digest, err)
			}
			img.blobs = append(img.blobs, blob)
		}
		return img
	}
	t.Fatalf("image %q not found in %s", ref, layoutDir)
	return fixtureImage{}
}

// toc returns layer i's table of contents, or false for a layer without one.
func (img fixtureImage) toc(t *testing.T, i int) (*zstdchunked.TOC, bool) {
	t.Helper()
	blob := img.blobs[i]
	pos, found, err := zstdchunked.Position(img.layers[i].Annotations, int64(len(blob)))
	if err != nil {
		t.Fatalf("Position(layer %d): %v", i, err)
	}
	if !found {
		return nil, false
	}
	toc, err := zstdchunked.Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
	if err != nil {
		t.Fatalf("Parse(layer %d): %v", i, err)
	}
	return toc, true
}

// open returns layer i's decompressed tar stream.
func (img fixtureImage) open(t *testing.T, i int) io.Reader {
	t.Helper()
	blob := bytes.NewReader(img.blobs[i])
	if strings.HasSuffix(img.layers[i].MediaType, "+gzip") {
		r, err := gzip.NewReader(blob)
		if err != nil {
			t.Fatalf("gzip layer %d: %v", i, err)
		}
		return r
	}
	r, err := zstd.NewReader(blob)
	if err != nil {
		t.Fatalf("zstd layer %d: %v", i, err)
	}
	t.Cleanup(r.Close)
	return r
}

func readFixtureJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads checked-in fixture paths
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}
