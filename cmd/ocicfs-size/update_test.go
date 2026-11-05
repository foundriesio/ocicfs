// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
