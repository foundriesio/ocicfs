// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/composefs"
)

// TestSavedLayerRoundTrip asserts a layer decoded from its encoding is the
// scanned layer record for record, with every stored payload pending, and
// that merging the decoded layers gives the entries and the canonical flag
// merging the scanned ones gives. The fixtures cover whiteouts, opaque
// directories, hardlinks, extended attributes, device nodes, and files on
// both sides of the inline threshold.
func TestSavedLayerRoundTrip(t *testing.T) {
	for name, layers := range savedFixtures(t) {
		t.Run(name, func(t *testing.T) {
			scanned, decoded := newMerge(), newMerge()
			for i, layer := range layers {
				diffID := digest.FromBytes(layer)
				blobSize := int64(len(layer))
				l, err := scanLayer(context.Background(), digestSink{}, bytes.NewReader(layer), diffID, i)
				if err != nil {
					t.Fatalf("scanLayer(%d): %v", i, err)
				}
				data, err := EncodeLayer(l, blobSize, diffID)
				if err != nil {
					t.Fatalf("EncodeLayer(%d): %v", i, err)
				}
				again, err := EncodeLayer(l, blobSize, diffID)
				if err != nil || !bytes.Equal(data, again) {
					t.Fatalf("EncodeLayer(%d) twice gave different bytes (err %v)", i, err)
				}
				got, err := DecodeLayer(data, i, blobSize, diffID)
				if err != nil {
					t.Fatalf("DecodeLayer(%d): %v", i, err)
				}
				assertSavedRecords(t, got, l)

				if err := l.resolve(); err != nil {
					t.Fatalf("resolve(%d): %v", i, err)
				}
				if err := scanned.merge(l); err != nil {
					t.Fatalf("merge scanned layer %d: %v", i, err)
				}
				if err := decoded.merge(got); err != nil {
					t.Fatalf("merge decoded layer %d: %v", i, err)
				}
			}
			want, wantCanonical, err := scanned.entries()
			if err != nil {
				t.Fatalf("entries of the scanned layers: %v", err)
			}
			got, gotCanonical, err := decoded.entries()
			if err != nil {
				t.Fatalf("entries of the decoded layers: %v", err)
			}
			// A decoded layer's stored payloads are pending: the builder
			// resolves their fs-verity digests against the store.
			for i := range want {
				want[i].Verity = ""
			}
			if !reflect.DeepEqual(got, want) || gotCanonical != wantCanonical {
				t.Fatalf("merged entries differ:\n got=%+v canonical=%v\nwant=%+v canonical=%v", got, gotCanonical, want, wantCanonical)
			}
		})
	}
}

// TestSavedLayerRoundTripsFixtureLayers asserts every layer of the
// zstd:chunked fixture layouts survives the encoding.
func TestSavedLayerRoundTripsFixtureLayers(t *testing.T) {
	for _, layout := range fixtureLayouts {
		for _, ref := range fixtureRefs(t, layout) {
			img := loadFixtureImage(t, layout, ref)
			for i := range img.layers {
				t.Run(fmt.Sprintf("%s/%s/layer%d", filepath.Base(layout), ref, i), func(t *testing.T) {
					l, err := scanLayer(context.Background(), digestSink{}, img.open(t, i), img.diffIDs[i], i)
					if err != nil {
						t.Skipf("the fixture layer does not scan: %v", err)
					}
					data, err := EncodeLayer(l, img.layers[i].Size, img.diffIDs[i])
					if err != nil {
						t.Fatalf("EncodeLayer: %v", err)
					}
					got, err := DecodeLayer(data, i, img.layers[i].Size, img.diffIDs[i])
					if err != nil {
						t.Fatalf("DecodeLayer: %v", err)
					}
					assertSavedRecords(t, got, l)
				})
			}
		}
	}
}

// TestSavedLayerBuildsTheSameImage asserts a builder fed decoded layers
// writes, out of the objects the scans stored, the composefs image the scans
// themselves build: the same descriptor, fs-verity digest, size and canonical
// flag, with no payload left to supply.
func TestSavedLayerBuildsTheSameImage(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	ctx := context.Background()
	for name, layers := range savedFixtures(t) {
		t.Run(name, func(t *testing.T) {
			store := newVerityStore(t)
			scanning := NewBuilder(store)
			defer scanning.Close()
			saved := make([][]byte, len(layers))
			for i, layer := range layers {
				diffID := digest.FromBytes(layer)
				l, err := scanning.Scan(ctx, bytes.NewReader(layer), diffID, i)
				skipIfNoVerity(t, err)
				if err != nil {
					t.Fatalf("Scan(%d): %v", i, err)
				}
				if saved[i], err = EncodeLayer(l, int64(len(layer)), diffID); err != nil {
					t.Fatalf("EncodeLayer(%d): %v", i, err)
				}
				if err := scanning.Merge(l); err != nil {
					t.Fatalf("Merge(%d): %v", i, err)
				}
			}
			want, err := scanning.Finish(ctx)
			if err != nil {
				t.Fatalf("Finish of the scanned layers: %v", err)
			}

			reusing := NewBuilder(store)
			defer reusing.Close()
			for i, layer := range layers {
				l, err := DecodeLayer(saved[i], i, int64(len(layer)), digest.FromBytes(layer))
				if err != nil {
					t.Fatalf("DecodeLayer(%d): %v", i, err)
				}
				if err := reusing.Merge(l); err != nil {
					t.Fatalf("Merge decoded layer %d: %v", i, err)
				}
			}
			missing, err := reusing.Missing(ctx)
			if err != nil {
				t.Fatalf("Missing: %v", err)
			}
			if len(missing) != 0 {
				t.Fatalf("Missing = %v, want none: the scans stored every file", missing)
			}
			got, err := reusing.Finish(ctx)
			if err != nil {
				t.Fatalf("Finish of the decoded layers: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("image built from decoded layers = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDecodeLayerPlacesNodesAtIndex asserts a saved layer is decoded for the
// position the caller names, whatever position it was scanned at.
func TestDecodeLayerPlacesNodesAtIndex(t *testing.T) {
	layer := metadataLayer(t)
	diffID := digest.FromBytes(layer)
	l, err := scanLayer(context.Background(), digestSink{}, bytes.NewReader(layer), diffID, 0)
	if err != nil {
		t.Fatalf("scanLayer: %v", err)
	}
	data, err := EncodeLayer(l, 1, diffID)
	if err != nil {
		t.Fatalf("EncodeLayer: %v", err)
	}
	got, err := DecodeLayer(data, 5, 1, diffID)
	if err != nil {
		t.Fatalf("DecodeLayer: %v", err)
	}
	if got.idx != 5 {
		t.Fatalf("decoded layer index = %d, want 5", got.idx)
	}
	for _, rec := range got.records {
		if rec.n != nil && rec.n.layer != 5 {
			t.Fatalf("%q decoded at layer %d, want 5", rec.name, rec.n.layer)
		}
	}
}

// TestDecodeLayerRejects asserts a saved layer is refused when it was written
// for another blob size or diff_id or in another version, when its frame is
// damaged, and when a record is not one a scan yields.
func TestDecodeLayerRejects(t *testing.T) {
	const blobSize = 1234
	diffID := digest.FromString("layer")
	hdr := savedHeader{Version: savedVersion, BlobSize: blobSize, DiffID: diffID}
	stored := digest.FromString("stored")
	valid := rawSaved(t, hdr,
		savedRecord{Kind: "node", Name: []byte("usr"), Mode: uint32(os.ModeDir | 0o755)},
		savedRecord{Kind: "node", Name: []byte("usr/small"), Mode: 0o644, Content: []byte("tiny"), Size: 4},
		savedRecord{Kind: "node", Name: []byte("usr/large"), Mode: 0o644, Digest: stored, Size: 100},
		savedRecord{Kind: "hardlink", Name: []byte("usr/alias"), Target: []byte("usr/large")},
		savedRecord{Kind: "whiteout", Name: []byte("usr/.wh.gone")},
		savedRecord{Kind: "opaque", Name: []byte("usr/.wh..wh..opq")},
	)
	l, err := DecodeLayer(valid, 0, blobSize, diffID)
	if err != nil {
		t.Fatalf("DecodeLayer of the valid layer: %v", err)
	}
	if got := l.records[4]; got.kind != kindWhiteout || got.target != "usr/gone" {
		t.Fatalf("whiteout record = %+v, want it to remove usr/gone", got)
	}

	t.Run("another diff_id", func(t *testing.T) {
		if _, err := DecodeLayer(valid, 0, blobSize, digest.FromString("other")); err == nil {
			t.Fatal("DecodeLayer accepted a layer saved for another diff_id")
		}
	})
	t.Run("another blob size", func(t *testing.T) {
		if _, err := DecodeLayer(valid, 0, blobSize+1, diffID); err == nil {
			t.Fatal("DecodeLayer accepted a layer saved for another blob size")
		}
	})

	damaged := map[string][]byte{
		"empty":            {},
		"not zstd":         []byte(`{"version":1}`),
		"truncated":        valid[:len(valid)-6],
		"trailing bytes":   append(append([]byte{}, valid...), "trailing"...),
		"flipped byte":     flipByte(valid, len(valid)/2),
		"flipped checksum": flipByte(valid, len(valid)-1),
		"unknown version":  rawSaved(t, savedHeader{Version: savedVersion + 1, BlobSize: blobSize, DiffID: diffID}),
		"no header":        rawSaved(t, nil, savedRecord{Kind: "node", Name: []byte("usr"), Mode: uint32(os.ModeDir | 0o755)}),
	}
	records := map[string]savedRecord{
		"unknown kind":                  {Kind: "socket", Name: []byte("a")},
		"no kind":                       {Name: []byte("a")},
		"empty name":                    {Kind: "node", Mode: 0o644},
		"escaping name":                 {Kind: "node", Name: []byte("../a"), Mode: 0o644},
		"absolute name":                 {Kind: "node", Name: []byte("/a"), Mode: 0o644},
		"unclean name":                  {Kind: "node", Name: []byte("a/./b"), Mode: 0o644},
		"name with NUL":                 {Kind: "node", Name: []byte("a\x00b"), Mode: 0o644},
		"file with no content":          {Kind: "node", Name: []byte("a"), Mode: 0o644, Size: 4},
		"content shorter than size":     {Kind: "node", Name: []byte("a"), Mode: 0o644, Content: []byte("abc"), Size: 4},
		"content longer than size":      {Kind: "node", Name: []byte("a"), Mode: 0o644, Content: []byte("abcde"), Size: 4},
		"inline over the threshold":     {Kind: "node", Name: []byte("a"), Mode: 0o644, Content: bytes.Repeat([]byte("a"), composefs.InlineContentMax+1), Size: composefs.InlineContentMax + 1},
		"malformed digest":              {Kind: "node", Name: []byte("a"), Mode: 0o644, Digest: "sha256:abc", Size: 100},
		"non-sha256 digest":             {Kind: "node", Name: []byte("a"), Mode: 0o644, Digest: digest.Digest("sha512:" + strings.Repeat("a", 128)), Size: 100},
		"stored at the threshold":       {Kind: "node", Name: []byte("a"), Mode: 0o644, Digest: stored, Size: composefs.InlineContentMax},
		"stored with a negative size":   {Kind: "node", Name: []byte("a"), Mode: 0o644, Digest: stored, Size: -1},
		"stored with content":           {Kind: "node", Name: []byte("a"), Mode: 0o644, Digest: stored, Content: []byte("x"), Size: 100},
		"directory with a payload":      {Kind: "node", Name: []byte("a"), Mode: uint32(os.ModeDir | 0o755), Digest: stored, Size: 100},
		"symlink with content":          {Kind: "node", Name: []byte("a"), Mode: uint32(os.ModeSymlink | 0o777), Linkname: []byte("b"), Content: []byte("x"), Size: 1},
		"unsupported type":              {Kind: "node", Name: []byte("a"), Mode: uint32(os.ModeSocket | 0o644)},
		"repeated extended attribute":   {Kind: "node", Name: []byte("a"), Mode: 0o644, Xattrs: []savedXattr{{Key: []byte("user.k"), Value: []byte("1")}, {Key: []byte("user.k"), Value: []byte("2")}}},
		"empty extended attribute key":  {Kind: "node", Name: []byte("a"), Mode: 0o644, Xattrs: []savedXattr{{Key: []byte(""), Value: []byte("1")}}},
		"NUL in extended attribute key": {Kind: "node", Name: []byte("a"), Mode: 0o644, Xattrs: []savedXattr{{Key: []byte("user.a\x00b"), Value: []byte("1")}}},
		"= in extended attribute key":   {Kind: "node", Name: []byte("a"), Mode: 0o644, Xattrs: []savedXattr{{Key: []byte("user.a=b"), Value: []byte("1")}}},
		"symlink with no target":        {Kind: "node", Name: []byte("a"), Mode: uint32(os.ModeSymlink | 0o777)},
		"file with a link target":       {Kind: "node", Name: []byte("a"), Mode: 0o644, Linkname: []byte("b")},
		"file with device numbers":      {Kind: "node", Name: []byte("a"), Mode: 0o644, Devmajor: 1},
		"mode bits no scan sets":        {Kind: "node", Name: []byte("a"), Mode: uint32(os.ModeAppend | 0o644)},
		"negative mtime":                {Kind: "node", Name: []byte("a"), Mode: 0o644, MtimeSec: -1},
		"nanoseconds out of range":      {Kind: "node", Name: []byte("a"), Mode: 0o644, MtimeNsec: 1e9},
		"node named as a whiteout":      {Kind: "node", Name: []byte("usr/.wh.a"), Mode: 0o644},
		"node named as an opaque":       {Kind: "node", Name: []byte("usr/.wh..wh..opq"), Mode: 0o644},
		"node with a target":            {Kind: "node", Name: []byte("a"), Mode: 0o644, Target: []byte("b")},

		// A scan derives what a whiteout removes from its name alone.
		"whiteout with a target":       {Kind: "whiteout", Name: []byte("usr/.wh.keep"), Target: []byte("etc")},
		"whiteout with its own target": {Kind: "whiteout", Name: []byte("usr/.wh.keep"), Target: []byte("usr/keep")},
		"whiteout not named as one":    {Kind: "whiteout", Name: []byte("usr/keep")},
		"whiteout of nothing":          {Kind: "whiteout", Name: []byte("usr/.wh.")},
		"whiteout named as an opaque":  {Kind: "whiteout", Name: []byte("usr/.wh..wh..opq")},
		"whiteout with a mode":         {Kind: "whiteout", Name: []byte("usr/.wh.keep"), Mode: 0o644},
		"opaque not named as one":      {Kind: "opaque", Name: []byte("usr/.wh.keep")},
		"opaque with any name":         {Kind: "opaque", Name: []byte("usr")},
		"opaque with a target":         {Kind: "opaque", Name: []byte("usr/.wh..wh..opq"), Target: []byte("usr")},
		"opaque with an mtime":         {Kind: "opaque", Name: []byte("usr/.wh..wh..opq"), MtimeSec: 1},
		"hardlink named as a whiteout": {Kind: "hardlink", Name: []byte("usr/.wh.a"), Target: []byte("b")},
		"hardlink to an unclean path":  {Kind: "hardlink", Name: []byte("a"), Target: []byte("../b")},
		"hardlink with a mode":         {Kind: "hardlink", Name: []byte("a"), Target: []byte("b"), Mode: 0o644},
		"hardlink with a digest":       {Kind: "hardlink", Name: []byte("a"), Target: []byte("b"), Digest: stored, Size: 100},
	}
	for name, rec := range records {
		damaged[name] = rawSaved(t, hdr, rec)
	}
	damaged["unknown field"] = rawSaved(t, hdr, map[string]any{"kind": "node", "name": []byte("a"), "mode": 0o644, "extra": true})
	for name, data := range damaged {
		t.Run(name, func(t *testing.T) {
			if l, err := DecodeLayer(data, 0, blobSize, diffID); err == nil {
				t.Fatalf("DecodeLayer accepted the layer: %+v", l.records)
			}
		})
	}
}

// TestSavedLayerBounds asserts a saved layer over the size or record bounds is
// refused on the way in — too large compressed, decompressing to too much, or
// holding too many records — and is never written on the way out.
func TestSavedLayerBounds(t *testing.T) {
	const blobSize = 1234
	diffID := digest.FromString("layer")
	hdr := savedHeader{Version: savedVersion, BlobSize: blobSize, DiffID: diffID}
	opaque := savedRecord{Kind: "opaque", Name: []byte("d/.wh..wh..opq")}

	t.Run("decode at the record limit", func(t *testing.T) {
		records := make([]any, savedMaxRecords)
		for i := range records {
			records[i] = opaque
		}
		l, err := DecodeLayer(rawSaved(t, hdr, records...), 0, blobSize, diffID)
		if err != nil || len(l.records) != savedMaxRecords {
			t.Fatalf("DecodeLayer of %d records: %v", savedMaxRecords, err)
		}
		if _, err := DecodeLayer(rawSaved(t, hdr, append(records, opaque)...), 0, blobSize, diffID); err == nil || !strings.Contains(err.Error(), "records") {
			t.Fatalf("DecodeLayer of %d records = %v, want the record limit", savedMaxRecords+1, err)
		}
	})
	t.Run("decode a frame that decompresses past the limit", func(t *testing.T) {
		// Whitespace between JSON values: a few kilobytes compressed.
		var buf bytes.Buffer
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(zw).Encode(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(bytes.Repeat([]byte(" "), savedMaxBytes)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if buf.Len() > 1<<20 {
			t.Fatalf("the frame is %d bytes compressed, not the small one the case means", buf.Len())
		}
		if _, err := DecodeLayer(buf.Bytes(), 0, blobSize, diffID); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
			t.Fatalf("DecodeLayer = %v, want the size limit", err)
		}
	})
	t.Run("decode more bytes than the limit", func(t *testing.T) {
		if _, err := DecodeLayer(make([]byte, savedMaxBytes+1), 0, blobSize, diffID); err == nil || !strings.Contains(err.Error(), "exceed the limit") {
			t.Fatalf("DecodeLayer = %v, want the size limit", err)
		}
	})
	t.Run("encode too many records", func(t *testing.T) {
		l := &Layer{records: make([]record, savedMaxRecords+1)}
		for i := range l.records {
			l.records[i] = record{kind: kindOpaque, name: "d/.wh..wh..opq"}
		}
		if _, err := EncodeLayer(l, blobSize, diffID); err == nil || !strings.Contains(err.Error(), "records") {
			t.Fatalf("EncodeLayer = %v, want the record limit", err)
		}
	})
	t.Run("encode too many bytes", func(t *testing.T) {
		// Each record carries a megabyte of extended attribute.
		value := strings.Repeat("x", 1<<20)
		l := &Layer{}
		for i := range savedMaxBytes>>20 + 1 {
			l.records = append(l.records, record{kind: kindNode, name: fmt.Sprintf("f%d", i), n: &node{mode: 0o644, content: []byte{}, xattrs: map[string]string{"user.big": value}}})
		}
		if _, err := EncodeLayer(l, blobSize, diffID); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
			t.Fatalf("EncodeLayer = %v, want the size limit", err)
		}
	})
}

// TestLayerPayloads asserts Payloads lists every stored file of a scanned
// layer by digest and size, and no inlined file.
func TestLayerPayloads(t *testing.T) {
	first, second := large("first"), large("second")
	layer := tarBytes(t,
		tarEntry{name: "d", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "d/first", typ: tar.TypeReg, mode: 0o644, body: first},
		tarEntry{name: "d/small", typ: tar.TypeReg, mode: 0o644, body: "tiny"},
		tarEntry{name: "d/alias", typ: tar.TypeLink, link: "d/first"},
		tarEntry{name: "d/second", typ: tar.TypeReg, mode: 0o644, body: second},
		tarEntry{name: "d/.wh.gone", typ: tar.TypeReg},
	)
	l, err := scanLayer(context.Background(), digestSink{}, bytes.NewReader(layer), digest.FromBytes(layer), 0)
	if err != nil {
		t.Fatalf("scanLayer: %v", err)
	}
	want := []Payload{
		{Digest: digest.FromString(first), Size: int64(len(first))},
		{Digest: digest.FromString(second), Size: int64(len(second))},
	}
	if got := l.Payloads(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Payloads = %v, want %v", got, want)
	}
}

// savedFixtures are the layer sets the scan-and-merge tests use, plus one
// layer of metadata a file list has to carry through bytes for bytes.
func savedFixtures(t *testing.T) map[string][][]byte {
	t.Helper()
	fixtures := scanMergeFixtures(t)
	fixtures["metadata"] = [][]byte{metadataLayer(t)}
	return fixtures
}

// metadataLayer is a layer whose names, link target and extended attributes
// are not valid UTF-8, with a binary security.capability value, device nodes,
// ownership, setuid and sticky bits, sub-second modification times, markers,
// and regular files of 0, 64 and 65 bytes.
func metadataLayer(t *testing.T) []byte {
	t.Helper()
	mtime := time.Unix(1700000000, 123456789)
	capability := "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\xff\xfe\x00\x00"
	headers := []struct {
		hdr  tar.Header
		body string
	}{
		{hdr: tar.Header{Name: "usr", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "usr/bin", Typeflag: tar.TypeDir, Mode: 0o2755, Uid: 1000, Gid: 2000}},
		{
			hdr: tar.Header{Name: "usr/bin/ping", Typeflag: tar.TypeReg, Mode: 0o4755, PAXRecords: map[string]string{
				"SCHILY.xattr.security.capability": capability,
				"SCHILY.xattr.user.note":           "plain",
				"SCHILY.xattr.user.\xff\xfe":       "\xc3\x28",
			}},
			body: strings.Repeat("p", composefs.InlineContentMax+1),
		},
		{hdr: tar.Header{Name: "usr/bin/caf\xe9", Typeflag: tar.TypeReg, Mode: 0o644}, body: strings.Repeat("c", composefs.InlineContentMax)},
		{hdr: tar.Header{Name: "usr/bin/empty", Typeflag: tar.TypeReg, Mode: 0o600}},
		{hdr: tar.Header{Name: "usr/bin/link\xff", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "caf\xe9"}},
		{hdr: tar.Header{Name: "usr/bin/hard\xfe", Typeflag: tar.TypeLink, Linkname: "usr/bin/caf\xe9"}},
		{hdr: tar.Header{Name: "tmp", Typeflag: tar.TypeDir, Mode: 0o1777}},
		{hdr: tar.Header{Name: "dev", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}},
		{hdr: tar.Header{Name: "dev/sda", Typeflag: tar.TypeBlock, Mode: 0o660, Gid: 6, Devmajor: 8, Devminor: 16}},
		{hdr: tar.Header{Name: "dev/pipe", Typeflag: tar.TypeFifo, Mode: 0o600}},
		{hdr: tar.Header{Name: "usr/bin/.wh.gone\xff", Typeflag: tar.TypeReg}},
		{hdr: tar.Header{Name: "tmp/.wh..wh..opq", Typeflag: tar.TypeReg}},
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		hdr := h.hdr
		hdr.Format = tar.FormatPAX
		hdr.ModTime = mtime
		hdr.Size = int64(len(h.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("WriteHeader %q: %v", hdr.Name, err)
		}
		if _, err := tw.Write([]byte(h.body)); err != nil {
			t.Fatalf("Write %q: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}
	return buf.Bytes()
}

// assertSavedRecords fails the test unless the decoded layer matches the
// scanned one record for record: the same kinds, names, targets and nodes,
// with a stored file's payload pending where the scan was sealing it.
func assertSavedRecords(t *testing.T, decoded, scanned *Layer) {
	t.Helper()
	if len(decoded.records) != len(scanned.records) {
		t.Fatalf("records = %d, want %d", len(decoded.records), len(scanned.records))
	}
	for i, want := range scanned.records {
		got := decoded.records[i]
		if got.kind != want.kind || got.name != want.name || got.target != want.target {
			t.Fatalf("record %d = {%d %q %q}, want {%d %q %q}", i, got.kind, got.name, got.target, want.kind, want.name, want.target)
		}
		if (got.n == nil) != (want.n == nil) {
			t.Fatalf("record %d (%q): node presence differs", i, want.name)
		}
		if want.n == nil {
			continue
		}
		g, w := *got.n, *want.n
		if g.sealed != nil || g.verity != "" {
			t.Errorf("record %d (%q): decoded payload is not pending", i, want.name)
		}
		w.sealed, w.verity = nil, ""
		if !reflect.DeepEqual(g, w) {
			t.Errorf("record %d (%q): node = %+v, want %+v", i, want.name, g, w)
		}
	}
}

// rawSaved writes a saved layer out of the given JSON values, so a test can
// make one EncodeLayer never would. A nil header is left out.
func rawSaved(t *testing.T, hdr any, records ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	enc := json.NewEncoder(zw)
	values := records
	if hdr != nil {
		values = append([]any{hdr}, records...)
	}
	for _, v := range values {
		if err := enc.Encode(v); err != nil {
			t.Fatalf("encode saved value: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zstd: %v", err)
	}
	return buf.Bytes()
}

// flipByte returns a copy of data with the byte at i inverted.
func flipByte(data []byte, i int) []byte {
	out := append([]byte{}, data...)
	out[i] ^= 0xff
	return out
}

// TestMergeEntriesMatchesBuildEntries asserts merging layers scanned one by
// one, or decoded from their saved form, yields the inventory BuildEntries
// builds from the same streams: identical for scanned layers, and identical
// but for the fs-verity digests a decoded layer leaves to the caller.
func TestMergeEntriesMatchesBuildEntries(t *testing.T) {
	ctx := context.Background()
	for name, layers := range savedFixtures(t) {
		t.Run(name, func(t *testing.T) {
			diffIDs := make([]digest.Digest, len(layers))
			for i, layer := range layers {
				diffIDs[i] = digest.FromBytes(layer)
			}
			want, wantCanonical, err := BuildEntries(ctx, digestSink{}, openTestLayers(t, layers...), diffIDs)
			if err != nil {
				t.Fatalf("BuildEntries: %v", err)
			}

			scanned := make([]*Layer, len(layers))
			decoded := make([]*Layer, len(layers))
			for i, layer := range layers {
				l, err := ScanLayer(ctx, digestSink{}, bytes.NewReader(layer), diffIDs[i], i)
				if err != nil {
					t.Fatalf("ScanLayer(%d): %v", i, err)
				}
				data, err := EncodeLayer(l, 0, diffIDs[i])
				if err != nil {
					t.Fatalf("EncodeLayer(%d): %v", i, err)
				}
				if decoded[i], err = DecodeLayer(data, i, 0, diffIDs[i]); err != nil {
					t.Fatalf("DecodeLayer(%d): %v", i, err)
				}
				scanned[i] = l
			}

			got, canonical, err := MergeEntries(scanned, func(digest.Digest) digest.Digest {
				t.Fatal("a scanned layer has no pending payload")
				return ""
			})
			if err != nil {
				t.Fatalf("MergeEntries(scanned): %v", err)
			}
			if !reflect.DeepEqual(got, want) || canonical != wantCanonical {
				t.Fatalf("MergeEntries(scanned) = %+v, %t; want %+v, %t", got, canonical, want, wantCanonical)
			}

			got, canonical, err = MergeEntries(decoded, func(d digest.Digest) digest.Digest { return d })
			if err != nil {
				t.Fatalf("MergeEntries(decoded): %v", err)
			}
			for i := range got {
				if got[i].Digest != "" && got[i].Verity != got[i].Digest {
					t.Fatalf("%s: Verity = %s, want the content digest %s", got[i].Path, got[i].Verity, got[i].Digest)
				}
				got[i].Verity = ""
			}
			stripped := slices.Clone(want)
			for i := range stripped {
				stripped[i].Verity = ""
			}
			if !reflect.DeepEqual(got, stripped) || canonical != wantCanonical {
				t.Fatalf("MergeEntries(decoded) = %+v, %t; want %+v, %t", got, canonical, stripped, wantCanonical)
			}
		})
	}
}

// TestMergeEntriesRejectsMisplacedLayer asserts a layer scanned for one
// position is not merged at another.
func TestMergeEntriesRejectsMisplacedLayer(t *testing.T) {
	layer := metadataLayer(t)
	l, err := ScanLayer(context.Background(), digestSink{}, bytes.NewReader(layer), digest.FromBytes(layer), 1)
	if err != nil {
		t.Fatalf("ScanLayer: %v", err)
	}
	if _, _, err := MergeEntries([]*Layer{l}, func(d digest.Digest) digest.Digest { return d }); err == nil {
		t.Fatal("MergeEntries accepted layer 1 at position 0")
	}
}
