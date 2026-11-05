// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSubtractModels(t *testing.T) {
	a, b, c, d := baseLayers(t)
	p := testProfile()
	set := func(images ...[][]byte) Models {
		roots := rootsOf(t, images...)
		res := func(br *Breakdown, why string) *ModelResult {
			if br == nil {
				return &ModelResult{Reason: why}
			}
			return &ModelResult{Available: true, Breakdown: br}
		}
		pair := func(f func([]*root, Profile) (*Breakdown, string)) *ModelPair {
			on := p
			on.Verity = true
			return &ModelPair{Off: res(f(roots, p)), On: res(f(roots, on))}
		}
		// File CAS/image needs ocicfs scan records; a snapshot model stands in.
		return Models{Overlay: res(overlaySize(roots, p)), EROFS: pair(erofsSize), CAS: pair(erofsSize), LayerCAS: pair(layerCasSize)}
	}
	one := set([][]byte{a, b, c})
	two := set([][]byte{a, b, c}, [][]byte{a, b, d})

	same := subtractModels(two, two)
	for _, m := range []*ModelResult{same.Overlay, same.EROFS.Off, same.EROFS.On, same.LayerCAS.On} {
		if !m.Available || *m.Breakdown != (Breakdown{}) {
			t.Errorf("x − x = %+v, want zero", m.Breakdown)
		}
	}

	diff := subtractModels(two, one)
	got, x, y := diff.Overlay, two.Overlay, one.Overlay
	if got.Total != x.Total-y.Total || got.Files != x.Files-y.Files || got.Units != x.Units-y.Units || got.Units != 1 {
		t.Errorf("overlay difference %+v from %+v and %+v", got.Breakdown, x.Breakdown, y.Breakdown)
	}
	if got.Reuse != nil || got.Unused != (UnusedStats{}) {
		t.Errorf("difference keeps reuse %v, unused %+v", got.Reuse, got.Unused)
	}
	if on := diff.EROFS.On; on.Verity != two.EROFS.On.Verity-one.EROFS.On.Verity || on.Verity <= 0 {
		t.Errorf("erofs verity difference %d", on.Verity)
	}

	gone := two
	gone.Overlay = &ModelResult{Reason: "why"}
	if m := subtractModels(gone, one).Overlay; m.Available || m.Reason != "why" {
		t.Errorf("unavailable minuend: %+v", m)
	}
	if m := subtractModels(one, gone).Overlay; m.Available || m.Reason != "why" {
		t.Errorf("unavailable subtrahend: %+v", m)
	}
}

// sizeUpdate runs the update command and returns its JSON report.
func sizeUpdate(t testing.TB, open opener, args ...string) *Report {
	t.Helper()
	var out, errOut bytes.Buffer
	argv := append([]string{"update", "--json", "--no-cache", "--platform", "linux/amd64"}, args...)
	if code := run(context.Background(), argv, strings.NewReader(""), &out, &errOut, open); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if rep.Update == nil {
		t.Fatal("no update block")
	}
	return &rep
}

// modelResults lists every model result of ms by model and variant.
func modelResults(ms Models) map[string]*ModelResult {
	return results(&Report{Models: ms})
}

// additive is a result's additive fields and counts, the ones a
// difference keeps.
func additive(m *ModelResult) Breakdown {
	b := *m.Breakdown
	b.Unused, b.Reuse = UnusedStats{}, nil
	return b
}

func TestUpdateUnchanged(t *testing.T) {
	a, b, c, _ := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}})
	up := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[0]).Update
	after := modelResults(up.After)
	for k, m := range modelResults(up.Before) {
		if additive(m) != additive(after[k]) {
			t.Errorf("%s: before %+v, after %+v", k, m.Breakdown, after[k].Breakdown)
		}
	}
	for _, ms := range []Models{up.Additional, up.Reclaimable} {
		for k, m := range modelResults(ms) {
			if !m.Available || *m.Breakdown != (Breakdown{}) {
				t.Errorf("%s: %+v, want zero", k, m.Breakdown)
			}
		}
	}
}

func TestUpdateSymmetry(t *testing.T) {
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, d}})
	ab := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1]).Update
	ba := sizeUpdate(t, newSpyOpener().open, "--from", refs[1], "--to", refs[0]).Update
	pairs := []struct {
		name string
		x, y Models
	}{
		{"additional = reclaimable reversed", ab.Additional, ba.Reclaimable},
		{"union", ab.Union, ba.Union},
		{"before = after reversed", ab.Before, ba.After},
	}
	for _, p := range pairs {
		y := modelResults(p.y)
		for k, m := range modelResults(p.x) {
			if additive(m) != additive(y[k]) {
				t.Errorf("%s: %s %+v, %+v", p.name, k, m.Breakdown, y[k].Breakdown)
			}
		}
	}
	// The two sets share a layer, so holding both takes less than the sum.
	if u, x, y := ab.Union.Overlay.Total, ab.Before.Overlay.Total, ab.After.Overlay.Total; u >= x+y || u <= max(x, y) {
		t.Errorf("overlay union %d of %d and %d", u, x, y)
	}
}

func TestUpdateConfigOnly(t *testing.T) {
	a, b, _, _ := baseLayers(t)
	dir := t.TempDir()
	refs := writeLayout(t, dir, testImage{layers: [][]byte{a, b}, label: "1"}, testImage{layers: [][]byte{a, b}, label: "2"})
	// The new image's manifest and config, each one block and one fs-verity
	// descriptor block on the host.
	for _, host := range []string{"include", "exclude"} {
		up := sizeUpdate(t, newSpyOpener().open, "--host-metadata", host, "--from", refs[0], "--to", refs[1]).Update
		add := modelResults(up.Additional)
		for _, k := range []string{"overlay", "erofs/off", "erofs/on", "layercas/off", "layercas/on"} {
			if add[k].Total != 0 || add[k].Files != 0 {
				t.Errorf("%s %s: +%d bytes, +%d files", host, k, add[k].Total, add[k].Files)
			}
		}
		off, on := add["cas/off"], add["cas/on"]
		if off.Files != 4 || on.Files != 4 {
			t.Errorf("%s: cas +%d/+%d files, want 4", host, off.Files, on.Files)
		}
		if off.OCIMeta != 2*4096 || on.Verity != 2*4096 || on.Total-off.Total != 2*4096 {
			t.Errorf("%s: cas metadata +%d, verity +%d", host, off.OCIMeta, on.Verity)
		}
		switch {
		case host == "exclude" && off.Total != 2*4096:
			t.Errorf("exclude: cas +%d bytes, want the two objects", off.Total)
		case host == "include" && off.Total <= 2*4096:
			t.Errorf("include: cas +%d bytes, want the objects and their host blocks", off.Total)
		}
	}
}

func TestUpdateRollback(t *testing.T) {
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	up := sizeUpdate(t, newSpyOpener().open, "--rollback", "--from", refs[0], "--to", refs[1]).Update
	union := modelResults(up.Union)
	for k, m := range modelResults(up.After) {
		if additive(m) != additive(union[k]) {
			t.Errorf("rollback %s: after %+v, union %+v", k, m.Breakdown, union[k].Breakdown)
		}
	}
	for k, m := range modelResults(up.Reclaimable) {
		if *m.Breakdown != (Breakdown{}) {
			t.Errorf("rollback %s reclaims %+v", k, m.Breakdown)
		}
	}

	up = sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1]).Update
	rec := modelResults(up.Reclaimable)
	if rec["cas/on"].PayloadBytes != 7000 || rec["overlay"].Units != 1 || rec["erofs/on"].Units != 1 {
		t.Errorf("reclaimable: cas %d payload bytes, overlay %d and erofs %d snapshots", rec["cas/on"].PayloadBytes, rec["overlay"].Units, rec["erofs/on"].Units)
	}
}

func TestUpdateReportJSON(t *testing.T) {
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	rep := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1], "--to", refs[0])
	if rep.Schema != 2 {
		t.Errorf("schema %d", rep.Schema)
	}
	up := rep.Update
	union, before, after := modelResults(up.Union), modelResults(up.Before), modelResults(up.After)
	for k, m := range modelResults(up.Additional) {
		if want := subtractBreakdown(union[k].Breakdown, before[k].Breakdown); additive(m) != *want {
			t.Errorf("additional %s: %+v, want %+v", k, m.Breakdown, want)
		}
	}
	for k, m := range modelResults(up.Reclaimable) {
		if want := subtractBreakdown(union[k].Breakdown, after[k].Breakdown); additive(m) != *want {
			t.Errorf("reclaimable %s: %+v, want %+v", k, m.Breakdown, want)
		}
	}
	var sets [][]string
	for _, img := range rep.Images {
		sets = append(sets, img.Sets)
	}
	if fmt.Sprint(sets) != "[[from to] [to]]" || len(up.From) != 1 || len(up.To) != 2 {
		t.Errorf("sets %v, from %v, to %v", sets, up.From, up.To)
	}
	if rep.Models.Overlay.Total != up.Union.Overlay.Total {
		t.Errorf("top-level models are not the union")
	}
}

func TestUpdateUnavailable(t *testing.T) {
	l1 := tarOf(t, dir("./"), file("a", strings.Repeat("a", 5000)))
	l2 := tarOf(t, hardlink("b", "a"))
	a, _, _, _ := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a}}, testImage{layers: [][]byte{l1, l2}})
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"update", "--no-cache", "--details", "--platform", "linux/amd64", "--from", refs[0], "--to", refs[1]}, nil, &out, &errOut, newSpyOpener().open); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	// The from image alone sizes every model, yet each table shows the
	// models the to image leaves unavailable as such.
	text, _, _ := strings.Cut(out.String(), "\nSTORAGE PROJECTION")
	var tables int
	for _, l := range strings.Split(text, "\n") {
		name, _, _ := strings.Cut(l, "  ")
		switch name {
		case "Overlay", "EROFS/layer", "File CAS/layer":
			if !strings.Contains(l, "n/a‡") {
				t.Errorf("row %q is not unavailable", l)
			}
			tables++
		case "File CAS/image":
			if strings.Contains(l, "n/a") {
				t.Errorf("row %q is unavailable", l)
			}
		}
	}
	if tables != 2*3 || strings.Count(text, "‡ Overlay, EROFS/layer, File CAS/layer unavailable:") != 1 {
		t.Errorf("%d unavailable rows:\n%s", tables, text)
	}
}

// TestUpdateCommaList checks a comma-separated --to sizes the same set
// as repeating the flag.
func TestUpdateCommaList(t *testing.T) {
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	repeated := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[0], "--to", refs[1])
	comma := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[0]+","+refs[1])
	got, err := json.Marshal(comma.Update)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(repeated.Update)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || len(comma.Images) != 2 {
		t.Errorf("comma list %s, repeated %s", got, want)
	}
}

func TestUpdateUsage(t *testing.T) {
	for _, argv := range [][]string{{"update", "--from", "x"}, {"update", "--to", "x"}, {"update", "--from", "x", "--to", "y", "z"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), argv, nil, &out, &errOut, newSpyOpener().open); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", argv, code, errOut.String())
		}
	}
}
