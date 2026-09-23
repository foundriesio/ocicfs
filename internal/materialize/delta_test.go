// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package materialize

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/rootfs"
	"github.com/foundriesio/knarr/internal/zstdchunked"
)

// The zstd:chunked fixture layouts, all built for linux/amd64.
var (
	fixturePlatform = ocispec.Platform{OS: "linux", Architecture: "amd64"}
	fixtureLayout   = filepath.Join("..", "zstdchunked", "testdata", "layout")
	releaseN        = filepath.Join("..", "zstdchunked", "testdata", "release-n")
	releaseN1       = filepath.Join("..", "zstdchunked", "testdata", "release-n1")
)

// TestDeltaMatchesMaterialize asserts a delta pull from an empty store builds,
// for every fixture image, the image Materialize builds, or fails when it
// does.
func TestDeltaMatchesMaterialize(t *testing.T) {
	requireTools(t)
	for _, c := range []struct{ dir, ref string }{{releaseN, "n"}, {releaseN1, "n1"}} {
		t.Run(c.ref, func(t *testing.T) {
			assertDeltaMatches(t, c.dir, c.ref)
		})
	}
	for _, ref := range layoutRefs(t, fixtureLayout) {
		t.Run(ref, func(t *testing.T) {
			assertDeltaMatches(t, fixtureLayout, ref)
		})
	}
}

func assertDeltaMatches(t *testing.T, dir, ref string) {
	t.Helper()
	root := fixtureRoot(t, dir, ref)
	wantRoot, wantObj, wantImg := newStores(t)
	want, wantErr := Materialize(context.Background(), wantObj, wantImg, openLayout(t, dir), root, fixturePlatform, newSlots(2))
	skipIfNoVerity(t, wantErr)
	gotRoot, gotObj, gotImg := newStores(t)
	got, gotErr := Delta(context.Background(), gotObj, gotImg, openLayout(t, dir), openLayout(t, dir), root, fixturePlatform, newSlots(2))
	if wantErr != nil {
		if gotErr == nil {
			t.Fatalf("Materialize failed (%v) but Delta succeeded", wantErr)
		}
		assertNoBinding(t, gotImg, root)
		return
	}
	if gotErr != nil {
		t.Fatalf("Delta: %v", gotErr)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)
}

// TestDeltaFetchesOnlyMissing asserts a delta pull into a store holding the
// previous release fetches every layer's table of contents once and then
// exactly the coalesced extents of the files the store lacks, and builds the
// image a whole pull builds.
func TestDeltaFetchesOnlyMissing(t *testing.T) {
	requireTools(t)
	rootN, rootN1 := fixtureRoot(t, releaseN, "n"), fixtureRoot(t, releaseN1, "n1")
	gotRoot, gotObj, gotImg := newStores(t)
	if _, err := Delta(context.Background(), gotObj, gotImg, openLayout(t, releaseN), openLayout(t, releaseN), rootN, fixturePlatform, newSlots(2)); err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("Delta(n): %v", err)
	}

	rec := newRecorder(t, releaseN1)
	got, err := Delta(context.Background(), gotObj, gotImg, rec, rec, rootN1, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Delta(n1): %v", err)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, releaseN1), rootN1, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize(n1): %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)

	// The base layer is held whole, so only its inline files (at most 64
	// bytes, which live in the image rather than the store) are fetched
	// again; the app layer lacks its changed inline file and its new file,
	// and its unchanged file is held.
	layers := fixtureLayers(t, releaseN1, "n1")
	want0 := extentOf(t, releaseN1, layers[0], "etc/inline", "etc/b63", "etc/b64", "run/pid")
	want1 := extentOf(t, releaseN1, layers[1], "app/config", "app/new")
	rec.assertRanges(t, []rangeReq{tocRange(t, layers[0]), tocRange(t, layers[1]), want0, want1})
	rec.assertOpens(t, nil)
}

// TestDeltaSkipsShadowedFiles asserts files a later layer removes or
// replaces are never fetched, and a digest listed twice is fetched once.
func TestDeltaSkipsShadowedFiles(t *testing.T) {
	requireTools(t)
	// Layer 0 holds f and g; layer 1 whites f out and re-adds it, re-adds g
	// and whites it out. Only layer 1's f survives, and layer 1's g shares
	// its digest.
	root := fixtureRoot(t, fixtureLayout, "whiteout-same-layer")
	rec := newRecorder(t, fixtureLayout)
	_, obj, img := newStores(t)
	if _, err := Delta(context.Background(), obj, img, rec, rec, root, fixturePlatform, newSlots(2)); err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("Delta: %v", err)
	}
	layers := fixtureLayers(t, fixtureLayout, "whiteout-same-layer")
	rec.assertRanges(t, []rangeReq{tocRange(t, layers[0]), tocRange(t, layers[1]), extentOf(t, fixtureLayout, layers[1], "f")})
	rec.assertOpens(t, nil)
}

// TestDeltaFetchesDigestFromLowestLayer asserts a payload two layers list is
// fetched from the lower one.
func TestDeltaFetchesDigestFromLowestLayer(t *testing.T) {
	requireTools(t)
	// release-n's base layer stores the same bytes under etc/subsecond as
	// hardlink-chain's only layer does under a.
	dir, root := composeImage(t, layerOf(t, releaseN, "n", 0), layerOf(t, fixtureLayout, "hardlink-chain", 0))
	rec := newRecorder(t, dir)
	gotRoot, gotObj, gotImg := newStores(t)
	got, err := Delta(context.Background(), gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
	if err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("Delta: %v", err)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, dir), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)

	layers := fixtureLayers(t, dir, "composed")
	toc0 := parseTOC(t, dir, layers[0])
	var names []string
	for _, e := range toc0.Entries {
		if e.Digest != "" {
			names = append(names, e.Name)
		}
	}
	rec.assertRanges(t, []rangeReq{tocRange(t, layers[0]), tocRange(t, layers[1]), extentOf(t, dir, layers[0], names...)})
}

// TestDeltaThresholdsFallBack asserts a layer whose plan trips the request or
// fraction bound is read whole, before any range of its content is asked
// for, and still builds the same image.
func TestDeltaThresholdsFallBack(t *testing.T) {
	requireTools(t)
	for _, c := range []struct {
		name string
		set  func()
	}{
		{"requests", func() { maxDeltaRequests = 0 }},
		{"fraction", func() { maxDeltaFraction = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			restoreBounds(t)
			c.set()
			rootN, rootN1 := fixtureRoot(t, releaseN, "n"), fixtureRoot(t, releaseN1, "n1")
			gotRoot, gotObj, gotImg := newStores(t)
			if _, err := Materialize(context.Background(), gotObj, gotImg, openLayout(t, releaseN), rootN, fixturePlatform, newSlots(2)); err != nil {
				skipIfNoVerity(t, err)
				t.Fatalf("Materialize(n): %v", err)
			}
			logs := captureLogs(t)
			rec := newRecorder(t, releaseN1)
			got, err := Delta(context.Background(), gotObj, gotImg, rec, rec, rootN1, fixturePlatform, newSlots(2))
			if err != nil {
				t.Fatalf("Delta(n1): %v", err)
			}
			wantRoot, wantObj, wantImg := newStores(t)
			want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, releaseN1), rootN1, fixturePlatform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize(n1): %v", err)
			}
			assertSameImage(t, wantRoot, want, gotRoot, got)

			layers := fixtureLayers(t, releaseN1, "n1")
			rec.assertRanges(t, []rangeReq{tocRange(t, layers[0]), tocRange(t, layers[1])})
			rec.assertOpens(t, []digest.Digest{layers[0].Digest, layers[1].Digest})
			if n := strings.Count(logs.String(), "fallback="+c.name); n != 2 {
				t.Fatalf("logged %d layers falling back for %s, want 2:\n%s", n, c.name, logs)
			}
		})
	}
}

// TestDeltaReadsFallbackLayersTogether asserts layers read whole are read
// under the slots limiter at once rather than one after another: two of the
// three layers, each with a stored file of its own to fetch, are held open
// until both have been opened, which a serial read never reaches.
func TestDeltaReadsFallbackLayersTogether(t *testing.T) {
	requireTools(t)
	restoreBounds(t)
	maxDeltaFraction = 0
	dir, root := composeImage(t, layerOf(t, releaseN, "n", 0), layerOf(t, releaseN1, "n1", 1), layerOf(t, fixtureLayout, "whiteout-same-layer", 1))
	layers := fixtureLayers(t, dir, "composed")
	rec := newRecorder(t, dir)
	rec.meet = map[digest.Digest]bool{layers[0].Digest: true, layers[2].Digest: true}
	rec.met = make(chan struct{})
	logs := captureLogs(t)
	gotRoot, gotObj, gotImg := newStores(t)
	// A pull that opened the layers one at a time would wait at the gate
	// forever; the deadline turns that into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := Delta(ctx, gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
	if err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("Delta: %v", err)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, dir), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)
	rec.assertOpens(t, []digest.Digest{layers[0].Digest, layers[1].Digest, layers[2].Digest})
	if n := strings.Count(logs.String(), "fallback=fraction"); n != 3 {
		t.Fatalf("logged %d layers falling back, want 3:\n%s", n, logs)
	}
}

// TestDeltaFallbackFailureStopsThePull asserts a failure in a layer read
// whole, while another layer is being fetched by ranges, is what the pull
// reports, and that it leaves no binding or temp; a rerun then builds the
// image Materialize builds.
func TestDeltaFallbackFailureStopsThePull(t *testing.T) {
	requireTools(t)
	restoreBounds(t)
	// One request per file, and at most two: the one-file layer is fetched
	// by range, the three-file layer is read whole.
	coalesceGap = -1
	maxDeltaRequests = 2
	dir, root := composeImage(t, layerOf(t, fixtureLayout, "whiteout-same-layer", 1), layerOf(t, releaseN1, "n1", 1))
	layers := fixtureLayers(t, dir, "composed")
	rec := newRecorder(t, dir)
	// Both tables come first; the third request is the ranged layer's file.
	// It is held until the pull is cancelled, and the whole read of the other
	// layer fails only once it is held, so the failure lands while a range is
	// in flight.
	rec.holdRange = 3
	rec.held = make(chan struct{})
	rec.failOpen = map[digest.Digest]error{layers[1].Digest: errInjected}
	// A pull that never reached the held request would wait here forever;
	// the deadline turns that into a failure.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rec.onOpen = func(d digest.Digest) {
		if d == layers[1].Digest {
			select {
			case <-rec.held:
			case <-ctx.Done():
			}
		}
	}
	slots := newSlots(2)
	gotRoot, gotObj, gotImg := newStores(t)
	_, err := Delta(ctx, gotObj, gotImg, rec, rec, root, fixturePlatform, slots)
	skipIfNoVerity(t, err)
	if !errors.Is(err, errInjected) {
		t.Fatalf("Delta returned %v, want the injected failure", err)
	}
	if ranges := rec.rangesOf(layers[0].Digest); len(ranges) != 2 {
		t.Fatalf("the ranged layer took %d requests, want its table and the held file", len(ranges))
	}
	if len(slots) != 0 {
		t.Fatalf("%d slots still taken after the failed pull", len(slots))
	}
	assertNoBinding(t, gotImg, root)
	assertNoTemps(t, gotObj)

	got, err := Delta(context.Background(), gotObj, gotImg, newRecorder(t, dir), newRecorder(t, dir), root, fixturePlatform, slots)
	if err != nil {
		t.Fatalf("Delta rerun: %v", err)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, dir), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)
}

// TestDeltaSourceCancellationIsAFailure asserts a source that gives up on a
// layer with a cancellation of its own, while the pull itself is not
// cancelled, fails the pull like any other error instead of leaving the
// layer out.
func TestDeltaSourceCancellationIsAFailure(t *testing.T) {
	requireTools(t)
	root := fixtureRoot(t, fixtureLayout, "mixed")
	layers := fixtureLayers(t, fixtureLayout, "mixed")
	rec := newRecorder(t, fixtureLayout)
	// The second layer has no table of contents and is opened whole.
	rec.failOpen = map[digest.Digest]error{layers[1].Digest: context.Canceled}
	gotRoot, gotObj, gotImg := newStores(t)
	_, err := Delta(context.Background(), gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
	skipIfNoVerity(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Delta returned %v, want the source's cancellation", err)
	}
	assertNoBinding(t, gotImg, root)
	assertNoTemps(t, gotObj)
	_ = gotRoot
}

// TestDeltaCancelledMidFetch asserts a pull cancelled while its files are
// being fetched, by range or whole, returns the cancellation, joins every
// goroutine, and leaves no binding or temp; a rerun then succeeds.
func TestDeltaCancelledMidFetch(t *testing.T) {
	requireTools(t)
	for _, c := range []struct {
		name     string
		fraction float64
	}{
		{"ranges", 0.5},
		{"whole", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			restoreBounds(t)
			coalesceGap = -1
			maxDeltaFraction = c.fraction
			root := fixtureRoot(t, releaseN1, "n1")
			layers := fixtureLayers(t, releaseN1, "n1")
			rec := newRecorder(t, releaseN1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Both tables come first; the third request is the first file's.
			// When the layers are read whole, the first layer's whole read is
			// the trigger; the metadata is opened through the same source
			// earlier and must not be.
			var reached atomic.Bool
			rec.onRange = func(n int) {
				if n == 3 {
					reached.Store(true)
					cancel()
				}
			}
			if c.fraction == 0 {
				rec.onRange = nil
				rec.onOpen = func(d digest.Digest) {
					if d == layers[0].Digest {
						reached.Store(true)
						cancel()
					}
				}
			}
			slots := newSlots(2)
			gotRoot, gotObj, gotImg := newStores(t)
			_, err := Delta(ctx, gotObj, gotImg, rec, rec, root, fixturePlatform, slots)
			skipIfNoVerity(t, err)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Delta returned %v, want context.Canceled", err)
			}
			if !reached.Load() {
				t.Fatal("the pull was not cancelled where the test meant to")
			}
			if len(slots) != 0 {
				t.Fatalf("%d slots still taken after the cancelled pull", len(slots))
			}
			assertNoBinding(t, gotImg, root)
			assertNoTemps(t, gotObj)

			got, err := Delta(context.Background(), gotObj, gotImg, openLayout(t, releaseN1), openLayout(t, releaseN1), root, fixturePlatform, slots)
			if err != nil {
				t.Fatalf("Delta rerun: %v", err)
			}
			wantRoot, wantObj, wantImg := newStores(t)
			want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, releaseN1), root, fixturePlatform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			assertSameImage(t, wantRoot, want, gotRoot, got)
		})
	}
}

// TestDeltaWholeAnswer asserts a source that answers a range request with
// the whole blob serves the layer from that one stream: spooled when it
// answers the table of contents request, and then reported as taking no
// range request, or sliced sequentially when it answers a file request, with
// no further range asked for either way.
func TestDeltaWholeAnswer(t *testing.T) {
	requireTools(t)
	for _, c := range []struct {
		name       string
		wholeFrom  int
		wantRanges int
	}{
		{"toc", 0, 1},
		{"file", 1, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			restoreBounds(t)
			// One request per file, so a whole answer to the first has more
			// requests to make redundant.
			coalesceGap = -1
			root := fixtureRoot(t, releaseN, "n")
			layers := fixtureLayers(t, releaseN, "n")
			rec := newRecorder(t, releaseN)
			rec.wholeFrom[layers[0].Digest] = c.wholeFrom
			logs := captureLogs(t)
			gotRoot, gotObj, gotImg := newStores(t)
			got, err := Delta(context.Background(), gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
			if err != nil {
				skipIfNoVerity(t, err)
				t.Fatalf("Delta: %v", err)
			}
			wantRoot, wantObj, wantImg := newStores(t)
			want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, releaseN), root, fixturePlatform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			assertSameImage(t, wantRoot, want, gotRoot, got)

			if base := len(rec.rangesOf(layers[0].Digest)); base != c.wantRanges {
				t.Fatalf("base layer took %d range requests, want %d", base, c.wantRanges)
			}
			if got := loggedField(t, logs.String(), layers[0].Digest, "requests"); c.wholeFrom == 0 && got != 0 {
				t.Fatalf("the spooled layer logged requests=%d, want 0", got)
			}
			if !strings.Contains(logs.String(), "fallback=whole-answer") {
				t.Fatalf("no whole-answer fallback logged:\n%s", logs)
			}
			rec.assertOpens(t, nil)
			assertNoTemps(t, gotObj)
		})
	}
}

// TestDeltaInterruptedThenRerun asserts a delta pull that fails after
// supplying a file records nothing, and a rerun into the same store builds
// the image a whole pull builds.
func TestDeltaInterruptedThenRerun(t *testing.T) {
	requireTools(t)
	restoreBounds(t)
	coalesceGap = -1
	root := fixtureRoot(t, releaseN1, "n1")
	// Both tables come first; the fourth request is the second file's.
	rec := newRecorder(t, releaseN1)
	rec.failAt = 4
	gotRoot, gotObj, gotImg := newStores(t)
	_, err := Delta(context.Background(), gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
	skipIfNoVerity(t, err)
	if err == nil || !errors.Is(err, errInjected) {
		t.Fatalf("interrupted Delta returned %v, want the injected failure", err)
	}
	assertNoBinding(t, gotImg, root)
	assertNoTemps(t, gotObj)

	got, err := Delta(context.Background(), gotObj, gotImg, openLayout(t, releaseN1), openLayout(t, releaseN1), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Delta rerun: %v", err)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, releaseN1), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)
}

// TestDeltaTOCFailureFallsBack asserts a table of contents that cannot be
// read turns its layer over to the tar stream with a warning: the pull then
// succeeds with the same image when the blob is sound, and fails on the
// layer digest when it is not.
func TestDeltaTOCFailureFallsBack(t *testing.T) {
	requireTools(t)
	t.Run("oversized-ulen", func(t *testing.T) {
		// The annotation promises a table larger than the cap, so it is
		// refused before a byte of it is fetched; the blob is untouched.
		dir := copyLayout(t, releaseN1)
		root := rewriteLayer(t, dir, "n1", 1, func(desc *ocispec.Descriptor, blob []byte) []byte {
			fields := strings.Split(desc.Annotations[zstdchunked.ManifestPositionKey], ":")
			fields[2] = fmt.Sprint(64<<20 + 1)
			desc.Annotations[zstdchunked.ManifestPositionKey] = strings.Join(fields, ":")
			return blob
		})
		assertTOCFallbackMatches(t, dir, root)
	})
	t.Run("rejected-entry", func(t *testing.T) {
		// A well-formed table with an entry the parser refuses; the blob is
		// rewritten consistently, so the tar stream is sound.
		dir := copyLayout(t, releaseN1)
		root := rewriteLayer(t, dir, "n1", 1, func(desc *ocispec.Descriptor, blob []byte) []byte {
			return mutateTOC(t, desc, blob, func(entries []map[string]any) {
				entries[0]["type"] = "socket"
			})
		})
		assertTOCFallbackMatches(t, dir, root)
	})
	t.Run("wrong-checksum", func(t *testing.T) {
		// A byte of the table flipped in place: the checksum rejects it and
		// the whole blob no longer hashes to its descriptor.
		dir := copyLayout(t, releaseN1)
		layers := fixtureLayers(t, releaseN1, "n1")
		pos, _, err := zstdchunked.Position(layers[1].Annotations, layers[1].Size)
		if err != nil {
			t.Fatalf("Position: %v", err)
		}
		path := blobPath(dir, layers[1].Digest)
		blob, err := os.ReadFile(path) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatalf("read layer: %v", err)
		}
		blob[pos.Offset+pos.CompressedLen/2] ^= 0xff
		if err := os.WriteFile(path, blob, 0o600); err != nil { // #nosec G703 -- test rewrites the layout it copied
			t.Fatalf("write layer: %v", err)
		}
		root := fixtureRoot(t, dir, "n1")
		logs := captureLogs(t)
		_, obj, img := newStores(t)
		_, err = Delta(context.Background(), obj, img, openLayout(t, dir), openLayout(t, dir), root, fixturePlatform, newSlots(2))
		skipIfNoVerity(t, err)
		if err == nil || !strings.Contains(err.Error(), "blob digest mismatch") {
			t.Fatalf("Delta returned %v, want a layer digest mismatch", err)
		}
		if !strings.Contains(logs.String(), "falling back to whole layer") {
			t.Fatalf("no fallback warning logged:\n%s", logs)
		}
		assertNoBinding(t, img, root)
	})
}

// assertTOCFallbackMatches asserts a delta pull of the image at root in dir
// warns about a table of contents, reads the layer whole, and builds the
// image Materialize builds.
func assertTOCFallbackMatches(t *testing.T, dir string, root digest.Digest) {
	t.Helper()
	logs := captureLogs(t)
	rec := newRecorder(t, dir)
	gotRoot, gotObj, gotImg := newStores(t)
	got, err := Delta(context.Background(), gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if !strings.Contains(logs.String(), "falling back to whole layer") {
		t.Fatalf("no fallback warning logged:\n%s", logs)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, dir), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)
	layers := fixtureLayers(t, dir, "n1")
	rec.assertOpens(t, []digest.Digest{layers[1].Digest})
	if ranges := rec.rangesOf(layers[1].Digest); len(ranges) > 1 {
		t.Fatalf("the fallen-back layer took %d range requests, want at most its table", len(ranges))
	}
	// The bytes read for a table that was then refused count as fetched,
	// on top of the whole blob the fallback reads.
	var tocBytes int64
	if pos, ok, err := zstdchunked.Position(layers[1].Annotations, layers[1].Size); err == nil && ok {
		tocBytes = pos.CompressedLen
	}
	if got, want := loggedField(t, logs.String(), layers[1].Digest, "fetched"), layers[1].Size+tocBytes; got != want {
		t.Fatalf("the fallen-back layer logged fetched=%d, want %d (blob %d + table attempt %d)", got, want, layers[1].Size, tocBytes)
	}
}

// loggedField returns the named figure of the layer's log line.
func loggedField(t *testing.T, logs string, layer digest.Digest, field string) int64 {
	t.Helper()
	m := regexp.MustCompile(`msg="delta: layer" layer=` + regexp.QuoteMeta(layer.String()) + ` .* ` + field + `=(\d+)`).FindStringSubmatch(logs)
	if m == nil {
		t.Fatalf("no log line for layer %s:\n%s", layer, logs)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("%s figure %q: %v", field, m[1], err)
	}
	return n
}

// TestDeltaMixedImage asserts a layer without a table of contents is scanned
// from its tar stream beside the layers fetched by range.
func TestDeltaMixedImage(t *testing.T) {
	requireTools(t)
	root := fixtureRoot(t, fixtureLayout, "mixed")
	rec := newRecorder(t, fixtureLayout)
	gotRoot, gotObj, gotImg := newStores(t)
	got, err := Delta(context.Background(), gotObj, gotImg, rec, rec, root, fixturePlatform, newSlots(2))
	if err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("Delta: %v", err)
	}
	wantRoot, wantObj, wantImg := newStores(t)
	want, err := Materialize(context.Background(), wantObj, wantImg, openLayout(t, fixtureLayout), root, fixturePlatform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	assertSameImage(t, wantRoot, want, gotRoot, got)
	layers := fixtureLayers(t, fixtureLayout, "mixed")
	rec.assertOpens(t, []digest.Digest{layers[1].Digest})
	if ranges := rec.rangesOf(layers[1].Digest); len(ranges) != 0 {
		t.Fatalf("the gzip layer took %d range requests", len(ranges))
	}
	if ranges := rec.rangesOf(layers[0].Digest); len(ranges) < 2 {
		t.Fatalf("the zstd:chunked layer took %d range requests, want its table and its files", len(ranges))
	}
}

// TestCheckSpaceRefusesOversizedPull asserts the storage-fit check refuses
// content the filesystem cannot hold and names the shortfall as a lower
// bound.
func TestCheckSpaceRefusesOversizedPull(t *testing.T) {
	obj := openObjectStore(t, t.TempDir())
	if err := checkSpace(obj, []rootfs.Payload{{Size: 1}}); err != nil {
		t.Fatalf("checkSpace(1 byte): %v", err)
	}
	err := checkSpace(obj, []rootfs.Payload{{Size: math.MaxInt64 / 2}, {Size: math.MaxInt64 / 2}})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("checkSpace(too much) = %v, want ErrNoSpace", err)
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Fatalf("error does not say the figure is a lower bound: %v", err)
	}
	// Sizes whose sum does not fit an int64 must not wrap into a small figure.
	err = checkSpace(obj, []rootfs.Payload{{Size: 1 << 62}, {Size: 1 << 62}, {Size: 1 << 62}})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("checkSpace(overflowing sum) = %v, want ErrNoSpace", err)
	}
}

// requireTools skips a test that needs mkcomposefs.
func requireTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
}

// restoreBounds restores the planner's bounds when the test ends.
func restoreBounds(t *testing.T) {
	t.Helper()
	gap, requests, fraction := coalesceGap, maxDeltaRequests, maxDeltaFraction
	t.Cleanup(func() { coalesceGap, maxDeltaRequests, maxDeltaFraction = gap, requests, fraction })
}

// captureLogs routes the default logger into a buffer until the test ends.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// syncBuffer is a bytes.Buffer safe for the goroutines a delta pull logs from.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// assertSameImage asserts two materializations built the same image: the
// same sealed composefs image, fs-verity digest, canonical verdict, and
// referenced object set.
func assertSameImage(t *testing.T, wantRoot string, want Built, gotRoot string, got Built) {
	t.Helper()
	if !sameBuilt(want, got) {
		t.Fatalf("built mismatch:\n want=%#v\n  got=%#v", want, got)
	}
	wantImg, gotImg := openImageStore(t, wantRoot), openImageStore(t, gotRoot)
	wantBinding, err := wantImg.Get(want.Identity.Digest, want.Platform)
	if err != nil {
		t.Fatalf("want binding: %v", err)
	}
	gotBinding, err := gotImg.Get(got.Identity.Digest, got.Platform)
	if err != nil {
		t.Fatalf("got binding: %v", err)
	}
	if !reflect.DeepEqual(wantBinding, gotBinding) {
		t.Fatalf("binding mismatch:\n want=%#v\n  got=%#v", wantBinding, gotBinding)
	}
	wantRefs := referencedObjects(t, openObjectStore(t, wantRoot), want.Descriptor.Digest)
	gotRefs := referencedObjects(t, openObjectStore(t, gotRoot), got.Descriptor.Digest)
	if !reflect.DeepEqual(wantRefs, gotRefs) {
		t.Fatalf("referenced objects mismatch:\n want=%v\n  got=%v", wantRefs, gotRefs)
	}
}

// referencedObjects returns the objects the composefs image d references,
// after checking that store holds each under its fs-verity view name.
func referencedObjects(t *testing.T, store *objectstore.Store, d digest.Digest) []string {
	t.Helper()
	path, err := store.ObjectPath(d)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	refs, err := composefs.ReferencedObjects(context.Background(), path)
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

func assertNoBinding(t *testing.T, img *imagestore.Store, root digest.Digest) {
	t.Helper()
	if _, err := img.Get(root, fixturePlatform); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("binding lookup after a failed pull = %v, want not found", err)
	}
}

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

func openObjectStore(t *testing.T, root string) *objectstore.Store {
	t.Helper()
	obj, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	return obj
}

func openImageStore(t *testing.T, root string) *imagestore.Store {
	t.Helper()
	img, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	return img
}

// rangeBlobSource is a blob source that serves ranges too, as a layout does.
type rangeBlobSource interface {
	ocichain.BlobSource
	ocichain.RangeSource
}

func openLayout(t *testing.T, dir string) rangeBlobSource {
	t.Helper()
	src, err := ocilayout.Open(dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	return src.(rangeBlobSource)
}

// rangeReq is one range request a recorder saw.
type rangeReq struct {
	blob   digest.Digest
	off, n int64
}

// errInjected is the failure a recorder injects into a range request.
var errInjected = errors.New("injected range failure")

// recorder wraps a layout source and records every Open and OpenRange. It can
// answer the ranges of a blob with the whole verified blob from a given
// request on, and fail the n-th content range request.
type recorder struct {
	src rangeBlobSource
	// wholeFrom answers a blob's range requests with the whole blob from the
	// given request index on (0 is the table of contents request).
	wholeFrom map[digest.Digest]int
	// failAt fails the n-th range request overall, counted from 1; 0 fails none.
	failAt int
	// failOpen fails every Open of the blobs in it with the given error.
	failOpen map[digest.Digest]error
	// onRange, when set, is told the count of range requests so far, before
	// each one is served; onOpen, when set, runs before each Open is served,
	// told the blob.
	onRange func(n int)
	onOpen  func(d digest.Digest)
	// meet holds the first Open of each blob in it until every one of them
	// has begun, so a pull that opens them one at a time never gets past the
	// first; the last arrival closes met.
	meet map[digest.Digest]bool
	met  chan struct{}
	// holdRange holds the n-th range request, counted from 1, until the
	// pull is cancelled; held is closed when that request arrives.
	holdRange int
	held      chan struct{}

	mu     sync.Mutex
	opens  []digest.Digest
	ranges []rangeReq
}

func newRecorder(t *testing.T, dir string) *recorder {
	t.Helper()
	return &recorder{src: openLayout(t, dir), wholeFrom: make(map[digest.Digest]int)}
}

func (r *recorder) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	r.mu.Lock()
	r.opens = append(r.opens, desc.Digest)
	meet := r.meet[desc.Digest]
	delete(r.meet, desc.Digest)
	if meet && len(r.meet) == 0 {
		close(r.met)
	}
	r.mu.Unlock()
	if meet {
		select {
		case <-r.met:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.onOpen != nil {
		r.onOpen(desc.Digest)
	}
	if err := r.failOpen[desc.Digest]; err != nil {
		return nil, err
	}
	return r.src.Open(ctx, desc)
}

func (r *recorder) OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	r.mu.Lock()
	r.ranges = append(r.ranges, rangeReq{blob: desc.Digest, off: off, n: n})
	total := len(r.ranges)
	var ofBlob int
	for _, rq := range r.ranges[:total-1] {
		if rq.blob == desc.Digest {
			ofBlob++
		}
	}
	r.mu.Unlock()
	if r.onRange != nil {
		r.onRange(total)
	}
	if r.failAt != 0 && total == r.failAt {
		return nil, false, errInjected
	}
	if r.holdRange != 0 && total == r.holdRange {
		close(r.held)
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	if from, ok := r.wholeFrom[desc.Digest]; ok && ofBlob >= from {
		stream, err := ocichain.OpenBlob(ctx, r.src, desc)
		return stream, true, err
	}
	return r.src.OpenRange(ctx, desc, off, n)
}

// rangesOf returns the range requests made for blob d, in order.
func (r *recorder) rangesOf(d digest.Digest) []rangeReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rangeReq
	for _, rq := range r.ranges {
		if rq.blob == d {
			out = append(out, rq)
		}
	}
	return out
}

// assertRanges asserts the recorder saw exactly want, in any order across
// blobs but in order within one.
func (r *recorder) assertRanges(t *testing.T, want []rangeReq) {
	t.Helper()
	r.mu.Lock()
	got := append([]rangeReq(nil), r.ranges...)
	r.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("range requests:\n got=%v\nwant=%v", got, want)
	}
	byBlob := func(reqs []rangeReq) map[digest.Digest][]rangeReq {
		m := make(map[digest.Digest][]rangeReq)
		for _, rq := range reqs {
			m[rq.blob] = append(m[rq.blob], rq)
		}
		return m
	}
	if !reflect.DeepEqual(byBlob(got), byBlob(want)) {
		t.Fatalf("range requests:\n got=%v\nwant=%v", got, want)
	}
}

// assertOpens asserts the recorder saw exactly the layer blobs in want
// opened whole, in any order, besides the metadata blobs every pull reads.
func (r *recorder) assertOpens(t *testing.T, want []digest.Digest) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []digest.Digest
	for _, d := range r.opens {
		if isLayerBlob(t, r.src, d) {
			got = append(got, d)
		}
	}
	want = append([]digest.Digest(nil), want...)
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("layer blobs opened whole:\n got=%v\nwant=%v", got, want)
	}
}

// isLayerBlob reports whether d names a blob that is not JSON metadata.
func isLayerBlob(t *testing.T, src ocichain.BlobSource, d digest.Digest) bool {
	t.Helper()
	f, err := src.Open(context.Background(), ocispec.Descriptor{Digest: d})
	if err != nil {
		t.Fatalf("open blob %s: %v", d, err)
	}
	defer f.Close() //nolint:errcheck // a test's read-only fixture blob
	var head [1]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		t.Fatalf("read blob %s: %v", d, err)
	}
	return head[0] != '{'
}

// tocRange is the range request that fetches desc's table of contents.
func tocRange(t *testing.T, desc ocispec.Descriptor) rangeReq {
	t.Helper()
	pos, ok, err := zstdchunked.Position(desc.Annotations, desc.Size)
	if err != nil || !ok {
		t.Fatalf("Position(%s): %v, %v", desc.Digest, ok, err)
	}
	return rangeReq{blob: desc.Digest, off: pos.Offset, n: pos.CompressedLen}
}

// extentOf is the one range request that covers the named files of desc's
// layer in dir: their extents lie within the coalescing gap of each other.
func extentOf(t *testing.T, dir string, desc ocispec.Descriptor, names ...string) rangeReq {
	t.Helper()
	toc := parseTOC(t, dir, desc)
	off, end := int64(math.MaxInt64), int64(0)
	for _, name := range names {
		var found bool
		for _, e := range toc.Entries {
			if e.Name != name {
				continue
			}
			found = true
			off, end = min(off, e.Offset), max(end, e.EndOffset)
		}
		if !found {
			t.Fatalf("entry %s not in the table of contents of %s", name, desc.Digest)
		}
	}
	return rangeReq{blob: desc.Digest, off: off, n: end - off}
}

func parseTOC(t *testing.T, dir string, desc ocispec.Descriptor) *zstdchunked.TOC {
	t.Helper()
	pos, ok, err := zstdchunked.Position(desc.Annotations, desc.Size)
	if err != nil || !ok {
		t.Fatalf("Position(%s): %v, %v", desc.Digest, ok, err)
	}
	blob, err := os.ReadFile(blobPath(dir, desc.Digest)) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read layer: %v", err)
	}
	toc, err := zstdchunked.Parse(blob[pos.Offset:pos.Offset+pos.CompressedLen], pos)
	if err != nil {
		t.Fatalf("Parse(%s): %v", desc.Digest, err)
	}
	return toc
}

func blobPath(dir string, d digest.Digest) string {
	return filepath.Join(dir, "blobs", d.Algorithm().String(), d.Encoded())
}

// layoutRefs lists the images a fixture layout holds.
func layoutRefs(t *testing.T, dir string) []string {
	t.Helper()
	var refs []string
	for _, desc := range readFixtureJSON[ocispec.Index](t, filepath.Join(dir, "index.json")).Manifests {
		refs = append(refs, desc.Annotations[ocispec.AnnotationRefName])
	}
	return refs
}

// fixtureRoot returns the manifest digest of the image ref in dir.
func fixtureRoot(t *testing.T, dir, ref string) digest.Digest {
	t.Helper()
	for _, desc := range readFixtureJSON[ocispec.Index](t, filepath.Join(dir, "index.json")).Manifests {
		if desc.Annotations[ocispec.AnnotationRefName] == ref {
			return desc.Digest
		}
	}
	t.Fatalf("image %q not found in %s", ref, dir)
	return ""
}

// fixtureLayers returns the layer descriptors of the image ref in dir.
func fixtureLayers(t *testing.T, dir, ref string) []ocispec.Descriptor {
	t.Helper()
	return readFixtureJSON[ocispec.Manifest](t, blobPath(dir, fixtureRoot(t, dir, ref))).Layers
}

// fixtureLayer is one layer of a fixture image, with the diff_id its config
// names for it and the layout it lives in.
type fixtureLayer struct {
	dir    string
	desc   ocispec.Descriptor
	diffID digest.Digest
}

func layerOf(t *testing.T, dir, ref string, i int) fixtureLayer {
	t.Helper()
	manifest := readFixtureJSON[ocispec.Manifest](t, blobPath(dir, fixtureRoot(t, dir, ref)))
	config := readFixtureJSON[ocispec.Image](t, blobPath(dir, manifest.Config.Digest))
	return fixtureLayer{dir: dir, desc: manifest.Layers[i], diffID: config.RootFS.DiffIDs[i]}
}

// composeImage writes a layout holding one image, "composed", made of the
// given fixture layers in order, and returns the layout and the image's
// manifest digest.
func composeImage(t *testing.T, layers ...fixtureLayer) (string, digest.Digest) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	var descs []ocispec.Descriptor
	var diffIDs []digest.Digest
	for _, l := range layers {
		blob, err := os.ReadFile(blobPath(l.dir, l.desc.Digest)) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatalf("read layer: %v", err)
		}
		writeFixtureBlob(t, dir, blob)
		descs = append(descs, l.desc)
		diffIDs = append(diffIDs, l.diffID)
	}
	config := ocispec.Image{
		Platform: fixturePlatform,
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	}
	configDesc := writeFixtureJSON(t, dir, config, ocispec.MediaTypeImageConfig)
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    descs,
	}
	manifest.SchemaVersion = 2
	manifestDesc := writeFixtureJSON(t, dir, manifest, ocispec.MediaTypeImageManifest)
	manifestDesc.Annotations = map[string]string{ocispec.AnnotationRefName: "composed"}
	index := ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{manifestDesc}}
	index.SchemaVersion = 2
	writeJSONFile(t, filepath.Join(dir, "index.json"), index)
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, manifestDesc.Digest
}

// copyLayout copies a fixture layout into a temp directory the test may
// rewrite.
func copyLayout(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(path) // #nosec G304 G122 -- test copies checked-in fixtures
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o600) // #nosec G703 -- test writes its own temp dir
	})
	if err != nil {
		t.Fatalf("copy layout: %v", err)
	}
	return dst
}

// rewriteLayer rewrites layer i of the image ref in dir through fn, which
// may change the descriptor's annotations and return new blob bytes, and
// re-derives the layer, manifest and index digests. It returns the new
// manifest digest.
func rewriteLayer(t *testing.T, dir, ref string, i int, fn func(desc *ocispec.Descriptor, blob []byte) []byte) digest.Digest {
	t.Helper()
	index := readFixtureJSON[ocispec.Index](t, filepath.Join(dir, "index.json"))
	var entry *ocispec.Descriptor
	for j := range index.Manifests {
		if index.Manifests[j].Annotations[ocispec.AnnotationRefName] == ref {
			entry = &index.Manifests[j]
		}
	}
	if entry == nil {
		t.Fatalf("image %q not found in %s", ref, dir)
	}
	manifest := readFixtureJSON[ocispec.Manifest](t, blobPath(dir, entry.Digest))
	desc := &manifest.Layers[i]
	blob, err := os.ReadFile(blobPath(dir, desc.Digest)) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read layer: %v", err)
	}
	blob = fn(desc, blob)
	desc.Digest, desc.Size = writeFixtureBlob(t, dir, blob), int64(len(blob))
	manifestDesc := writeFixtureJSON(t, dir, manifest, ocispec.MediaTypeImageManifest)
	entry.Digest, entry.Size = manifestDesc.Digest, manifestDesc.Size
	writeJSONFile(t, filepath.Join(dir, "index.json"), index)
	return manifestDesc.Digest
}

// mutateTOC rewrites the table of contents in blob through fn, recompresses
// it in place, and updates the descriptor's checksum and position
// annotations, so the layer stays consistent with its descriptor.
func mutateTOC(t *testing.T, desc *ocispec.Descriptor, blob []byte, fn func(entries []map[string]any)) []byte {
	t.Helper()
	pos, ok, err := zstdchunked.Position(desc.Annotations, desc.Size)
	if err != nil || !ok {
		t.Fatalf("Position(%s): %v, %v", desc.Digest, ok, err)
	}
	decoder, err := zstd.NewReader(bytes.NewReader(blob[pos.Offset : pos.Offset+pos.CompressedLen]))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	data, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	raw, ok := doc["entries"].([]any)
	if !ok {
		t.Fatal("manifest entries are not an array")
	}
	entries := make([]map[string]any, len(raw))
	for i, e := range raw {
		if entries[i], ok = e.(map[string]any); !ok {
			t.Fatalf("manifest entry %d is not an object", i)
		}
	}
	fn(entries)
	doc["entries"] = entries
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), blob[:pos.Offset]...)
	binary.LittleEndian.PutUint32(out[pos.Offset-4:], uint32(compressed.Len())) // #nosec G115 -- a fixture table is tiny
	out = append(out, compressed.Bytes()...)
	out = append(out, blob[pos.Offset+pos.CompressedLen:]...)
	desc.Annotations[zstdchunked.ManifestChecksumKey] = digest.FromBytes(compressed.Bytes()).String()
	desc.Annotations[zstdchunked.ManifestPositionKey] = fmt.Sprintf("%d:%d:%d:1", pos.Offset, compressed.Len(), len(data))
	return out
}

func writeFixtureBlob(t *testing.T, dir string, data []byte) digest.Digest {
	t.Helper()
	d := digest.FromBytes(data)
	if err := os.WriteFile(blobPath(dir, d), data, 0o600); err != nil { // #nosec G703 -- test writes its own temp layout
		t.Fatalf("write blob: %v", err)
	}
	return d
}

func writeFixtureJSON(t *testing.T, dir string, value any, mediaType string) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: writeFixtureBlob(t, dir, data), Size: int64(len(data))}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFixtureJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads fixture paths
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}
