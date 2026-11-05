// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestScanLayerRecordsEntries(t *testing.T) {
	raw := tarOf(t, dir("./"), file("etc/a", "hello"), hardlink("etc/b", "etc/a"), symlink("l", "etc/a"),
		whiteout("gone"), file("d/.wh..wh..opq", ""), file(".wh..wh.plnk.1", ""),
		tent{name: "x", body: "", xattrs: map[string]string{"user.b": "2", "security.capability": "c"}})
	inv := layerInv(t, raw)
	if inv.TarSize != int64(len(raw)) || inv.DiffID != digest.FromBytes(raw).String() {
		t.Fatalf("tar size %d diffID %s", inv.TarSize, inv.DiffID)
	}
	got := map[string]Kind{}
	for _, e := range inv.Entries {
		got[e.Path] = e.Kind
	}
	want := map[string]Kind{"/": KindDir, "/etc/a": KindReg, "/etc/b": KindHardlink, "/l": KindSymlink, "/gone": KindWhiteout, "/d": KindOpaque, "/x": KindReg}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: kind %d, want %d", p, got[p], k)
		}
	}
	if len(inv.Entries) != len(want) {
		t.Errorf("%d entries, want %d", len(inv.Entries), len(want))
	}
	if b := inv.Entries[2]; b.Link != 1 {
		t.Errorf("hardlink leader %d, want 1", b.Link)
	}
	if a := inv.Entries[1]; a.Size != 5 || a.Digest != digest.FromString("hello").Encoded() {
		t.Errorf("regular file %+v", a)
	}
	if x := inv.Entries[len(inv.Entries)-1]; len(x.Xattrs) != 2 || x.Xattrs[0].Name != "security.capability" {
		t.Errorf("xattrs %+v", x.Xattrs)
	}
	if len(inv.Notes) != 1 {
		t.Errorf("notes %v", inv.Notes)
	}
	if inv.Unsupported != "" {
		t.Errorf("unsupported %q", inv.Unsupported)
	}
}

func TestScanLayerMarksCrossLayerHardlink(t *testing.T) {
	inv := layerInv(t, tarOf(t, hardlink("b", "a")))
	if !strings.Contains(inv.Unsupported, "cross-layer hardlink") {
		t.Fatalf("unsupported %q", inv.Unsupported)
	}
}

func TestScanLayerLinksNonRegularInodes(t *testing.T) {
	inv := layerInv(t, tarOf(t, symlink("s", "t"), hardlink("a", "s"), hardlink("b", "a"), tent{name: "fifo", typ: tar.TypeFifo}, hardlink("c", "fifo")))
	if inv.Unsupported != "" {
		t.Fatalf("unsupported %q", inv.Unsupported)
	}
	for i, want := range map[int]int32{1: 0, 2: 0, 4: 3} {
		if got := inv.Entries[i].Link; got != want {
			t.Errorf("%s links entry %d, want %d", inv.Entries[i].Path, got, want)
		}
	}
	inv = layerInv(t, tarOf(t, dir("d/"), hardlink("x", "d")))
	if !strings.Contains(inv.Unsupported, "hardlink to a directory") {
		t.Errorf("unsupported %q", inv.Unsupported)
	}
}

func TestScanLayerRejects(t *testing.T) {
	for name, raw := range map[string][]byte{
		"escape":    tarOf(t, file("../../etc/passwd", "x")),
		"bare-wh":   tarOf(t, file("d/.wh.", "")),
		"xattr-big": tarOf(t, tent{name: "f", xattrs: map[string]string{"user.big": strings.Repeat("v", maxXattrValue+1)}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := scanLayer(context.Background(), io.NopCloser(bytes.NewReader(raw)), digest.FromBytes(raw)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	raw := tarOf(t, file("a", "x"))
	if _, err := scanLayer(context.Background(), io.NopCloser(bytes.NewReader(raw)), digest.FromString("other")); err == nil {
		t.Fatal("diff_id mismatch accepted")
	}
}

func TestScanLayerReadsToStreamEnd(t *testing.T) {
	raw := append(tarOf(t, file("a", "x")), make([]byte, 10*512)...)
	if inv := layerInv(t, raw); inv.TarSize != int64(len(raw)) {
		t.Fatalf("tar size %d, want %d", inv.TarSize, len(raw))
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(tarOf(t, file("a", strings.Repeat("x", 100000))))
	_ = zw.Close()
	cut := gz.Bytes()[:gz.Len()/2]
	_, err := scanLayer(context.Background(), mustGunzip(t, cut), digest.FromString("unknown"))
	if err == nil {
		t.Fatal("truncated stream accepted")
	}
}

func mustGunzip(t *testing.T, b []byte) io.ReadCloser {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

// fates returns each stored name's fate in a root, by layer and path.
func fates(r *root) map[string]uint8 {
	out := map[string]uint8{}
	for li, m := range r.mats {
		for i, n := range m.names {
			out[string(rune('0'+li))+n.path] = r.view.fate[li][i]
		}
	}
	return out
}

func TestReplayFates(t *testing.T) {
	l1 := tarOf(t, dir("./"), file("a", "1"), dir("d/"), file("d/x", "1"), file("d/y", "1"), file("f", "1"), dir("t/"), file("t/z", "1"), symlink("s", "a"), file("keep", "1"))
	l2 := tarOf(t, whiteout("a"), file("d/.wh..wh..opq", ""), file("d/y", "2"), dir("f/"), file("t", "now a file"), file("s", "file"), file("keep", "k"), whiteout("keep"))
	r := rootsOf(t, [][]byte{l1, l2})[0]
	f := fates(r)
	for p, want := range map[string]uint8{
		"0/a": fateWhiteouted, "0/d/x": fateWhiteouted, "0/d/y": fateWhiteouted, "0/d": fateVisible,
		"0/f": fateSuperseded, "0/t": fateSuperseded, "0/t/z": fateSuperseded, "0/s": fateSuperseded,
		"0/keep": fateWhiteouted, "1/keep": fateVisible, "1/d/y": fateVisible, "1/t": fateVisible,
	} {
		if f[p] != want {
			t.Errorf("%s: fate %d, want %d", p, f[p], want)
		}
	}
}

func TestMaterializeSameTarReplacement(t *testing.T) {
	m := materialize(layerInv(t, tarOf(t, file("a", strings.Repeat("x", 9000)), file("a", "y"))))
	if len(m.names) != 1 || m.entry(0).Size != 1 {
		t.Fatalf("names %+v", m.names)
	}
	m = materialize(layerInv(t, tarOf(t, file("a", strings.Repeat("x", 9000)), hardlink("b", "a"), file("a", "y"))))
	var sizes []int64
	for i := range m.names {
		sizes = append(sizes, m.entry(i).Size)
	}
	if len(sizes) != 2 || sizes[0]+sizes[1] != 9001 {
		t.Fatalf("sizes %v", sizes)
	}
	m = materialize(layerInv(t, tarOf(t, dir("d/"), file("d/x", "x"), file("d", "f"))))
	if len(m.names) != 1 || m.names[0].path != "/d" {
		t.Fatalf("names %+v", m.names)
	}
}

func TestImplicitDirsTakeLowerXattrs(t *testing.T) {
	p1 := tarOf(t, tent{name: "d/", typ: '5', xattrs: map[string]string{"user.a": "1"}})
	p2 := tarOf(t, dir("d/"))
	l := tarOf(t, file("d/file", "f"))
	roots := rootsOf(t, [][]byte{p1, l}, [][]byte{p2, l}, [][]byte{l})
	if im := roots[0].view.implicit[1]; len(im) != 1 || im[0].path != "/d" || len(im[0].xattrs) != 1 {
		t.Errorf("over p1: %+v", im)
	}
	if im := roots[1].view.implicit[1]; len(im) != 1 || len(im[0].xattrs) != 0 {
		t.Errorf("over p2: %+v", im)
	}
	if im := roots[2].view.implicit[0]; len(im) != 1 || len(im[0].xattrs) != 0 {
		t.Errorf("alone: %+v", im)
	}
}

func TestReplayRefusesSymlinkTraversal(t *testing.T) {
	same := tarOf(t, dir("real/"), symlink("alias", "real"), file("alias/file", "x"))
	if r := rootsOf(t, [][]byte{same})[0]; !strings.Contains(r.view.unsupported, "beneath symlink") {
		t.Errorf("same layer: %q", r.view.unsupported)
	}
	lower := tarOf(t, dir("real/"), symlink("alias", "real"))
	upper := tarOf(t, file("alias/file", "x"))
	if r := rootsOf(t, [][]byte{lower, upper})[0]; !strings.Contains(r.view.unsupported, "beneath symlink") {
		t.Errorf("across layers: %q", r.view.unsupported)
	}
}

func TestLoadImagesReadsEachLayerOnce(t *testing.T) {
	stampVersion(t, "v1.0.0", "0123abc")
	layer := func(s string) []byte { return tarOf(t, file(s, strings.Repeat(s, 5000))) }
	a, b, c, d := layer("a"), layer("b"), layer("c"), layer("d")
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	cache := t.TempDir()
	load := func(refs ...string) (*spyOpener, *layerSet) {
		sp, err := newSpool("")
		if err != nil {
			t.Fatal(err)
		}
		defer sp.close()
		spy := newSpyOpener()
		ls := newLayerSet(cache)
		_, err = loadImages(context.Background(), spy.open, sp, ls, refs, platform, manifestKey, map[string]*Image{})
		if err != nil {
			t.Fatal(err)
		}
		return spy, ls
	}
	spy, ls := load(refs...)
	for d, n := range spy.opens {
		if n != 1 {
			t.Errorf("blob %s read %d times", d, n)
		}
	}
	if ls.scans != 4 || ls.ocicfsScans != 4 {
		t.Errorf("%d scans, %d ocicfs scans", ls.scans, ls.ocicfsScans)
	}
	if l, err := ls.ocicfsLayer(context.Background(), digest.FromBytes(d).String(), 2); err != nil || l == nil {
		t.Errorf("ocicfs layer: %v", err)
	}
	if _, ls = load(refs...); ls.scans != 0 || ls.ocicfsScans != 0 || ls.invHits != 4 || ls.recordHits != 4 {
		t.Errorf("cached: %d scans, %d ocicfs scans, %d+%d hits", ls.scans, ls.ocicfsScans, ls.invHits, ls.recordHits)
	}
}

func TestInvCacheKeepsBytes(t *testing.T) {
	ls := &layerSet{dir: t.TempDir()}
	diffID := digest.FromString("layer")
	capability := []Xattr{{Name: "security.capability", Value: "\x01\x00\x00\x02\x00\x20\x00\x00\xff"}}
	want := &LayerInv{Schema: scanSchema, DiffID: diffID.String(), Entries: []Entry{
		{Path: "/a\x80", Kind: KindReg, Link: -1},
		{Path: "/a\x81", Kind: KindSymlink, Link: -1, LinkName: "\xff\xfe", Xattrs: capability},
	}}
	ls.writeInv(want)
	if got := ls.readInv(diffID); !reflect.DeepEqual(got, want) {
		t.Errorf("cached inventory = %+v, want %+v", got, want)
	}
}

func TestRecordsRevision(t *testing.T) {
	for _, c := range []struct{ v, r, want string }{
		{"v1.0.0", "abc", "abc"}, {"dev", "abc", ""}, {"v1.0.0", "none", ""}, {"v1.0.0-dirty", "abc", ""}, {"", "abc", ""},
	} {
		if got := recordsRevision(c.v, c.r); got != c.want {
			t.Errorf("recordsRevision(%q, %q) = %q", c.v, c.r, got)
		}
	}
}

func TestReadRefs(t *testing.T) {
	refs, err := readRefs([]string{"a", "@-"}, strings.NewReader("# fleet\n\nb\n c \n"))
	if err != nil || strings.Join(refs, ",") != "a,b,c" {
		t.Fatalf("refs %v, %v", refs, err)
	}
}
