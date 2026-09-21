// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/zstdchunked"
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
