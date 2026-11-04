// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/composefs"
)

const kib = 1024

// testProfile is the profile with fs-verity on.
func testProfile() Profile {
	return Profile{HostBlock: 4096, ErofsBlock: 4096, VerityBlock: 4096, InlineMax: 64, Verity: true, HostMetadata: true}
}

func TestAllocAndVerityBoundaries(t *testing.T) {
	for _, c := range []struct{ s, want int64 }{{0, 0}, {1, 4096}, {4095, 4096}, {4096, 4096}, {4097, 8192}} {
		if got := alloc(c.s, 4096); got != c.want {
			t.Errorf("alloc(%d) = %d, want %d", c.s, got, c.want)
		}
	}
	if alloc(5000, 4096) == alloc(5000, 16384) {
		t.Error("block size has no effect")
	}
	for _, c := range []struct{ s, want int64 }{
		{0, 4 * kib},
		{1, 4 * kib},
		{4096, 4 * kib},
		{4097, 8 * kib},
		{512 * kib, 8 * kib},
		{512*kib + 1, 16 * kib},
		{64 << 20, 4*kib + 128*4*kib + 4*kib},
		{64<<20 + 1, 4*kib + 129*4*kib + 2*4*kib + 4*kib},
	} {
		if got := verityBytes(c.s, 4096, 4096); got != c.want {
			t.Errorf("V(%d) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestExt4DirBlocks(t *testing.T) {
	if got := ext4DirBlocks(nil); got != 1 {
		t.Errorf("empty dir: %d blocks", got)
	}
	// 4084 usable bytes, 24 for . and ..: 253 names of 8 bytes (16 each)
	// fit one block, 254 do not.
	names := func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = 8
		}
		return out
	}
	if got := ext4DirBlocks(names(253)); got != 1 {
		t.Errorf("253 names: %d blocks", got)
	}
	if got := ext4DirBlocks(names(254)); got != 1+2 {
		t.Errorf("254 names: %d blocks", got)
	}
	if ext4RecLen(1) != 12 || ext4RecLen(4) != 12 || ext4RecLen(5) != 16 {
		t.Error("rec_len rounding")
	}
}

func TestSnapshotHostMetadata(t *testing.T) {
	p := testProfile()
	one := rootsOf(t, [][]byte{tarOf(t, dir("./"), file("e", ""))})
	two := rootsOf(t, [][]byte{tarOf(t, dir("./"), file("e", "")), tarOf(t, file("f", ""))})
	for _, c := range []struct {
		name          string
		roots         []*root
		overlay, lcas int64
	}{
		{"one", one, 3 + 1, 3 + 1 + 1 + 1},
		{"two", two, 6 + 1, 6 + 2 + 1 + 1},
	} {
		o, _ := overlaySize(c.roots, p)
		e, _ := erofsSize(c.roots, p)
		l, _ := layerCasSize(c.roots, p)
		if o.Host != c.overlay*4096 || e.Host != c.overlay*4096 {
			t.Errorf("%s: overlay host %d, erofs host %d, want %d blocks", c.name, o.Host/4096, e.Host/4096, c.overlay)
		}
		if l.Host != c.lcas*4096 {
			t.Errorf("%s: layer CAS host %d blocks, want %d", c.name, l.Host/4096, c.lcas)
		}
	}
}

func TestHardlinksAndDuplicates(t *testing.T) {
	p := testProfile()
	body := strings.Repeat("h", 10*kib)
	linked := rootsOf(t, [][]byte{tarOf(t, file("a", body), hardlink("b", "a"))})
	dups := rootsOf(t, [][]byte{tarOf(t, file("a", body), file("b", body))})
	o, _ := overlaySize(linked, p)
	e, _ := erofsSize(linked, p)
	l, _ := layerCasSize(linked, p)
	if o.PayloadAlloc != 12*kib || e.PayloadAlloc != 12*kib || l.Objects != 1 {
		t.Errorf("hardlinks: overlay %d erofs %d objects %d", o.PayloadAlloc, e.PayloadAlloc, l.Objects)
	}
	o, _ = overlaySize(dups, p)
	l, _ = layerCasSize(dups, p)
	if o.PayloadAlloc != 24*kib || l.Objects != 1 || l.PayloadAlloc != 12*kib {
		t.Errorf("duplicates: overlay %d objects %d object bytes %d", o.PayloadAlloc, l.Objects, l.PayloadAlloc)
	}
	hidden := rootsOf(t, [][]byte{tarOf(t, file("a", body), hardlink("b", "a")), tarOf(t, whiteout("a"))})
	if o, _ = overlaySize(hidden, p); o.Unused.Versions != 0 {
		t.Errorf("a hidden hardlink name made the payload unused: %+v", o.Unused)
	}
}

func TestHardlinkedSymlinkIsOneInode(t *testing.T) {
	p := testProfile()
	target := strings.Repeat("p", 100)
	one := rootsOf(t, [][]byte{tarOf(t, dir("./"), symlink("s", target))})
	linked := rootsOf(t, [][]byte{tarOf(t, dir("./"), symlink("s", target), hardlink("h", "s"))})
	a, why := overlaySize(one, p)
	b, why2 := overlaySize(linked, p)
	if why != "" || why2 != "" {
		t.Fatalf("unavailable: %q %q", why, why2)
	}
	if a.Host != b.Host {
		t.Errorf("a hardlinked slow symlink changed host blocks: %d -> %d", a.Host, b.Host)
	}
	if _, why = erofsSize(linked, p); why != "" {
		t.Errorf("erofs unavailable: %q", why)
	}
}

func TestSharedPrefixes(t *testing.T) {
	p := testProfile()
	layer := func(s string) []byte { return tarOf(t, file(s, strings.Repeat(s, 5000))) }
	a, b, c, d, x := layer("a"), layer("b"), layer("c"), layer("d"), layer("x")
	roots := rootsOf(t, [][]byte{a, b, c}, [][]byte{a, b, d})
	if o, _ := overlaySize(roots, p); o.Units != 4 {
		t.Errorf("A,B,C + A,B,D: %d snapshots", o.Units)
	}
	roots = rootsOf(t, [][]byte{a, b}, [][]byte{x, b})
	o, _ := overlaySize(roots, p)
	l, _ := layerCasSize(roots, p)
	if o.Units != 4 || l.Units != 4 || l.Objects != 3 {
		t.Errorf("A,B + X,B: overlay %d snapshots, layer CAS %d snapshots and %d objects", o.Units, l.Units, l.Objects)
	}
	e, _ := erofsSize(roots, p)
	if o.Files != 4 || e.Files != 4 || l.Files != 4+4+3 {
		t.Errorf("A,B + X,B files: overlay %d, erofs %d, layer CAS %d", o.Files, e.Files, l.Files)
	}
}

func TestOversizedXattrsMakeOverlayUnavailable(t *testing.T) {
	p := testProfile()
	many := map[string]string{}
	for i := range 60 {
		many[fmt.Sprintf("user.k%02d", i)] = strings.Repeat("v", 100)
	}
	for name, x := range map[string]map[string]string{"one-large": {"user.big": strings.Repeat("v", 8*kib)}, "many-small": many} {
		roots := rootsOf(t, [][]byte{tarOf(t, tent{name: "f", body: "x", xattrs: x})})
		if b, why := overlaySize(roots, p); b != nil || !strings.Contains(why, "ENOSPC") {
			t.Errorf("%s: overlay %v %q", name, b, why)
		}
		if b, why := erofsSize(roots, p); b == nil {
			t.Errorf("%s: erofs unavailable: %s", name, why)
		}
	}
}

func TestUnusedAccounting(t *testing.T) {
	p := testProfile()
	l1 := tarOf(t, file("a", strings.Repeat("a", 5000)), file("b", strings.Repeat("b", 5000)), file("c", strings.Repeat("c", 100)))
	l2 := tarOf(t, whiteout("a"), file("b", "new"))
	roots := rootsOf(t, [][]byte{l1, l2})
	o, _ := overlaySize(roots, p)
	if u := o.Unused; u.Versions != 2 || u.Whiteouted != 1 || u.Superseded != 1 || u.Bytes != 10000 || u.Alloc != 16*kib {
		t.Errorf("overlay unused %+v", u)
	}
	e, _ := erofsSize(roots, p)
	if e.Unused != o.Unused {
		t.Errorf("erofs unused %+v", e.Unused)
	}
	l, _ := layerCasSize(roots, p)
	if l.Unused.Versions != 2 || l.Objects != 4 {
		t.Errorf("layer CAS unused %+v, %d objects", l.Unused, l.Objects)
	}

	// Visible in a second root sharing the snapshot: used.
	roots = rootsOf(t, [][]byte{l1, l2}, [][]byte{l1})
	if o, _ := overlaySize(roots, p); o.Unused.Versions != 0 {
		t.Errorf("rollback root: unused %+v", o.Unused)
	}
}

func TestCrossLayerHardlinkUnavailable(t *testing.T) {
	p := testProfile()
	roots := rootsOf(t, [][]byte{tarOf(t, file("a", strings.Repeat("a", 5000))), tarOf(t, hardlink("b", "a"))})
	for name, f := range map[string]func([]*root, Profile) (*Breakdown, string){"overlay": overlaySize, "erofs": erofsSize, "layercas": layerCasSize} {
		if b, why := f(roots, p); b != nil || !strings.Contains(why, "cross-layer hardlink") {
			t.Errorf("%s: %v %q", name, b, why)
		}
	}
}

func TestLayerCASObjects(t *testing.T) {
	p := testProfile()
	l1 := tarOf(t, file("empty", ""), file("small", strings.Repeat("s", 64)), file("gone", strings.Repeat("g", 100)))
	l2 := tarOf(t, whiteout("gone"), file("same", strings.Repeat("s", 64)))
	l, _ := layerCasSize(rootsOf(t, [][]byte{l1, l2}), p)
	if l.Objects != 2 || l.PayloadBytes != 164 || l.Verity != (2+2)*4096 { // two objects, two layer images
		t.Errorf("objects %d bytes %d verity %d", l.Objects, l.PayloadBytes, l.Verity)
	}
	if l.Unused.Versions != 1 {
		t.Errorf("unused %+v", l.Unused)
	}
}

func TestBreakdownTotal(t *testing.T) {
	b := &Breakdown{PayloadAlloc: 1, Layout: 2, Verity: 4, OCIMeta: 8, Host: 16, PayloadBytes: 1}
	b.total(Profile{HostMetadata: true})
	if b.Total != 31 {
		t.Errorf("total %d", b.Total)
	}
	b.total(Profile{})
	if b.Total != 15 {
		t.Errorf("total without host %d", b.Total)
	}
}

// casRoots loads layout images and prepares them as ocicfs would.
func casRoots(t *testing.T, images ...testImage) []*casRoot {
	t.Helper()
	return casRootsInline(t, composefs.InlineContentMax, images...)
}

// casRootsInline is casRoots with another inline threshold.
func casRootsInline(t *testing.T, inlineMax int64, images ...testImage) []*casRoot {
	t.Helper()
	ctx := context.Background()
	sp, err := newSpool("")
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	refs := writeLayout(t, t.TempDir(), images...)
	imgs, ls, err := loadImages(ctx, newSpyOpener().open, sp, "", refs, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	var out []*casRoot
	for _, img := range imgs {
		cr, err := prepareCASRoot(ctx, ls, img, inlineMax)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cr)
	}
	return out
}

func TestCASSize(t *testing.T) {
	p := testProfile()
	layer := tarOf(t, dir("./"), dir("usr/"), file("i64", strings.Repeat("a", 64)), file("e65", strings.Repeat("b", 65)), file("empty", ""))
	roots := casRoots(t, testImage{layers: [][]byte{layer}, label: "1"}, testImage{layers: [][]byte{layer}, label: "2"})
	if roots[0].id != roots[1].id || !roots[0].canonical {
		t.Errorf("config-only change: images %s %s, canonical %t", roots[0].id, roots[1].id, roots[0].canonical)
	}
	b := casSize(roots[:1], p)
	img := roots[0].img
	meta := alloc(img.ManifestSize, 4096) + alloc(img.ConfigSize, 4096)
	if b.Units != 1 || b.Objects != 1 || b.PayloadAlloc != 4096 || b.PayloadBytes != 65 || b.Inline != 64 || b.OCIMeta != meta {
		t.Errorf("one root: %+v", b)
	}
	if b.Files != 1+2+1+1+1 { // object, manifest and config, image, binding, reference
		t.Errorf("one root: %d files", b.Files)
	}
	if want := verityBytes(roots[0].size, 4096, 4096) + 3*4096; b.Verity != want {
		t.Errorf("verity %d, want %d", b.Verity, want)
	}
	both := casSize(roots, p)
	if both.Units != 1 || both.OCIMeta != meta+alloc(roots[1].img.ManifestSize, 4096)+alloc(roots[1].img.ConfigSize, 4096) {
		t.Errorf("two roots: %+v", both)
	}
	if both.Host <= b.Host {
		t.Errorf("a second root adds no binding: %d <= %d", both.Host, b.Host)
	}
	p.Verity = false
	if none := casSize(roots[:1], p); none.Verity != 0 {
		t.Errorf("verity off: %d", none.Verity)
	}
}

func TestCASImageIdentity(t *testing.T) {
	base := []tent{dir("./"), dir("usr/"), file("usr/f", "inline")}
	variants := [][]tent{
		{dir("./"), dir("usr/"), file("usr/f", "inlinf")},
		{dir("./"), dir("usr/"), tent{name: "usr/f", body: "inline", xattrs: map[string]string{"security.capability": "x"}}},
		{dir("./"), dir("usr/"), tent{name: "usr/f", body: "inline", mtime: 1}},
	}
	want := casRoots(t, testImage{layers: [][]byte{tarOf(t, base...)}})[0].id
	for i, v := range variants {
		if got := casRoots(t, testImage{layers: [][]byte{tarOf(t, v...)}})[0].id; got == want {
			t.Errorf("variant %d keeps the image identity", i)
		}
	}
}

func TestCASInlineMax(t *testing.T) {
	var files []tent
	sizes := []int{0, 1, 64, 65, 4096, 5000, 6000}
	for _, n := range sizes {
		files = append(files, file(fmt.Sprintf("usr/f%d", n), strings.Repeat("x", n)))
	}
	layer := tarOf(t, append([]tent{dir("./"), dir("usr/")}, files...)...)
	var id string
	for _, c := range []struct {
		max     int64
		objects int
		inline  int64
	}{
		{0, 6, 0},
		{64, 4, 1 + 64},
		{65, 3, 1 + 64 + 65},
		{5000, 1, 1 + 64 + 65 + 4096 + 5000},
		{1 << 20, 0, 1 + 64 + 65 + 4096 + 5000 + 6000},
	} {
		p := testProfile()
		p.InlineMax = c.max
		r := casRootsInline(t, c.max, testImage{layers: [][]byte{layer}})
		b := casSize(r, p)
		if b.Objects != c.objects || b.Inline != c.inline {
			t.Errorf("inline max %d: %d objects, %d inline bytes; want %d, %d", c.max, b.Objects, b.Inline, c.objects, c.inline)
		}
		if id != "" && r[0].id != id {
			t.Errorf("inline max %d changes the image identity", c.max)
		}
		id = r[0].id
	}
}

func TestComposefsInlineBeyondCap(t *testing.T) {
	// 20000 bytes inline are four full blocks and a 3616-byte tail, over
	// half a block, so a fifth block: five blocks more than an empty file.
	size := func(n int64) int64 {
		t.Helper()
		got, err := composefsImageSize([]composefs.Entry{{Path: "/", Mode: os.ModeDir | 0o755}, {Path: "/big", Mode: 0o644, Size: n, Digest: digest.FromString("big"), Verity: digest.FromString("big")}}, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := size(20000) - size(0); got != 5*4096 {
		t.Errorf("20000 inline bytes take %d, want %d", got, 5*4096)
	}
}

func TestCASSharesObjectsAcrossImages(t *testing.T) {
	p := testProfile()
	shared := strings.Repeat("s", 1000)
	base := tarOf(t, dir("./"), dir("usr/"), file("usr/one", "1"))
	other := tarOf(t, dir("./"), dir("usr/"), file("usr/two", "2"))
	roots := casRoots(t,
		testImage{layers: [][]byte{base, tarOf(t, file("usr/a", shared))}},
		testImage{layers: [][]byte{other, tarOf(t, dir("opt/")), tarOf(t, file("opt/b", shared))}})
	if roots[0].id == roots[1].id {
		t.Fatal("the images are not distinct")
	}
	b := casSize(roots, p)
	var meta, verity int64
	for _, r := range roots {
		for _, n := range []int64{r.img.ManifestSize, r.img.ConfigSize} {
			meta += alloc(n, 4096)
			verity += verityBytes(n, 4096, 4096)
		}
		verity += verityBytes(r.size, 4096, 4096)
	}
	verity += verityBytes(1000, 4096, 4096)
	if b.Units != 2 || b.Objects != 1 || b.PayloadAlloc != 4096 || b.PayloadBytes != 1000 || b.OCIMeta != meta || b.Verity != verity {
		t.Errorf("units %d objects %d payload %d/%d meta %d verity %d, want 2 1 4096/1000 %d %d", b.Units, b.Objects, b.PayloadAlloc, b.PayloadBytes, b.OCIMeta, b.Verity, meta, verity)
	}
}

func TestCASInlineFilePaidPerImage(t *testing.T) {
	p := testProfile()
	small := strings.Repeat("i", 40)
	roots := casRoots(t,
		testImage{layers: [][]byte{tarOf(t, dir("./"), dir("usr/"), file("usr/s", small), file("usr/one", "1"))}},
		testImage{layers: [][]byte{tarOf(t, dir("./"), dir("usr/"), file("usr/s", small), file("usr/two", "22"))}})
	if roots[0].id == roots[1].id {
		t.Fatal("the images are not distinct")
	}
	for i, r := range roots {
		if one := casSize([]*casRoot{r}, p); one.Objects != 0 || one.Inline != int64(40+1+i) {
			t.Errorf("root %d: %d objects, %d inline bytes", i, one.Objects, one.Inline)
		}
	}
	if b := casSize(roots, p); b.Units != 2 || b.Objects != 0 || b.Inline != 2*40+1+2 {
		t.Errorf("units %d objects %d inline %d", b.Units, b.Objects, b.Inline)
	}

	// 20000 bytes inline take five blocks (four full and a tail over half a
	// block) in each image, measured against the same file left empty.
	p.InlineMax = 1 << 20
	images := func(body string) []*casRoot {
		return casRootsInline(t, p.InlineMax,
			testImage{layers: [][]byte{tarOf(t, dir("./"), dir("usr/"), file("usr/big", body), file("usr/one", "1"))}},
			testImage{layers: [][]byte{tarOf(t, dir("./"), dir("usr/"), file("usr/big", body), file("usr/two", "22"))}})
	}
	with, without := images(strings.Repeat("b", 20000)), images("")
	for i := range with {
		if got := with[i].size - without[i].size; got != 5*4096 {
			t.Errorf("root %d grows by %d bytes, want %d", i, got, 5*4096)
		}
	}
	bw, bo := casSize(with, p), casSize(without, p)
	if got := bw.Layout - bo.Layout; got != 2*5*4096 || bw.Objects != 0 || bw.Units != 2 {
		t.Errorf("layout grows by %d, want %d; %d objects, %d images", got, 2*5*4096, bw.Objects, bw.Units)
	}
}
