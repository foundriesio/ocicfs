// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// reuseOf sizes images given as layer tars with every model and returns
// each model's reuse, keyed as results keys them, fs-verity on.
func reuseOf(t *testing.T, inlineMax string, images ...testImage) map[string]*Reuse {
	t.Helper()
	refs := writeLayout(t, t.TempDir(), images...)
	rep := sizeImages(t, newSpyOpener().open, append([]string{"--no-cache", "--inline-max", inlineMax}, refs...)...)
	out := map[string]*Reuse{}
	for k, m := range results(rep) {
		if !m.Available || m.Reuse == nil {
			t.Fatalf("%s: available %t, reuse %v", k, m.Available, m.Reuse)
		}
		if k == "overlay" || strings.HasSuffix(k, "/on") {
			out[strings.TrimSuffix(k, "/on")] = m.Reuse
		}
	}
	return out
}

func TestReuseSharedLayers(t *testing.T) {
	a, b, c, d := baseLayers(t)
	e := tarOf(t, file("extra", strings.Repeat("e", 3000)))
	got := reuseOf(t, "64", testImage{layers: [][]byte{a, b, c, e}}, testImage{layers: [][]byte{a, b, c, d}})
	shared := int64(300 + 20000 + 9000 + 1 + 7000)
	for _, k := range []string{"overlay", "erofs", "layercas"} {
		if r := got[k]; r.UnitsReferenced != 8 || r.UnitsReused != 3 || r.UnitBytes != shared {
			t.Errorf("%s: %+v", k, r)
		}
	}
	if r := got["cas"]; r.UnitsReferenced != 2 || r.UnitsReused != 0 || r.UnitBytes != 0 {
		t.Errorf("cas: %+v", r)
	}

	// Two images merging to one rootfs share one composefs image.
	got = reuseOf(t, "64", testImage{layers: [][]byte{a, b}, label: "1"}, testImage{layers: [][]byte{a, b}, label: "2"})
	if r := got["cas"]; r.UnitsReferenced != 2 || r.UnitsReused != 1 || r.UnitBytes != 300+20000+9000+1 {
		t.Errorf("config-only change: cas %+v", r)
	}
	if r := got["overlay"]; r.UnitsReferenced != 4 || r.UnitsReused != 2 {
		t.Errorf("config-only change: overlay %+v", r)
	}
}

func TestReuseContents(t *testing.T) {
	body := strings.Repeat("h", 10*kib)
	size := int64(10 * kib)
	base := []tent{dir("./"), dir("usr/")}
	reused := Reuse{UnitsReferenced: 1, Contents: 1, ContentBytes: size}
	duplicated := Reuse{UnitsReferenced: 1, Duplicated: 1, ExtraCopies: 1, ExtraBytes: size}
	for _, c := range []struct {
		name     string
		layers   [][]byte
		overlay  Reuse // overlay and EROFS/layer
		fileCAS  Reuse // File CAS/image and File CAS/layer
		snapshot int
	}{
		{"hardlinked names", [][]byte{tarOf(t, append(base, file("usr/a", body), hardlink("usr/b", "usr/a"))...)}, reused, reused, 1},
		{"two inodes, one layer", [][]byte{tarOf(t, append(base, file("usr/a", body), file("usr/b", body))...)}, duplicated, reused, 1},
		{"two layers", [][]byte{tarOf(t, append(base, file("usr/a", body))...), tarOf(t, dir("usr/"), file("usr/b", body))}, duplicated, reused, 2},
		{"empty files", [][]byte{tarOf(t, append(base, file("usr/a", ""), file("usr/b", ""), hardlink("usr/c", "usr/a"))...)}, Reuse{UnitsReferenced: 1}, Reuse{UnitsReferenced: 1}, 1},
	} {
		got := reuseOf(t, "64", testImage{layers: c.layers})
		for k, want := range map[string]Reuse{"overlay": c.overlay, "erofs": c.overlay, "cas": c.fileCAS, "layercas": c.fileCAS} {
			if k != "cas" {
				want.UnitsReferenced = c.snapshot
			}
			if *got[k] != want {
				t.Errorf("%s: %s: %+v, want %+v", c.name, k, *got[k], want)
			}
		}
	}
}

func TestReuseInlineAcrossImages(t *testing.T) {
	image := func(body, other string) testImage {
		return testImage{layers: [][]byte{
			tarOf(t, dir("./"), dir("usr/"), file("usr/"+other, other)),
			tarOf(t, dir("usr/"), file("usr/s", body)),
		}}
	}
	for _, c := range []struct {
		body          string
		inlineMax     string
		casDuplicated bool // inline in both composefs images: one copy each
	}{
		{body: strings.Repeat("i", 40), inlineMax: "64", casDuplicated: true},
		{body: strings.Repeat("i", 100), inlineMax: "64"},
		{body: strings.Repeat("i", 100), inlineMax: "100", casDuplicated: true},
	} {
		// The content's layer differs between the images, as its tar
		// carries a different mtime.
		a, b := image(c.body, "one"), image(c.body, "two")
		b.layers[1] = tarOf(t, dir("usr/"), tent{name: "usr/s", body: c.body, mtime: 1})
		got := reuseOf(t, c.inlineMax, a, b)
		size := int64(len(c.body))
		cas := Reuse{UnitsReferenced: 2, Contents: 1, ContentBytes: size}
		if c.casDuplicated {
			cas = Reuse{UnitsReferenced: 2, Duplicated: 1, ExtraCopies: 1, ExtraBytes: size}
		}
		if *got["cas"] != cas {
			t.Errorf("%d B, --inline-max %s: cas %+v, want %+v", size, c.inlineMax, *got["cas"], cas)
		}
		if l := got["layercas"]; l.Contents != 1 || l.ContentBytes != size || l.Duplicated != 0 {
			t.Errorf("%d B: layercas %+v", size, *l)
		}
		if o := got["overlay"]; o.Duplicated != 1 || o.Contents != 0 {
			t.Errorf("%d B: overlay %+v", size, *o)
		}
	}
}
