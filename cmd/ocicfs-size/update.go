// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

// subtractModels returns a − b, model by model and fs-verity variant by
// variant. Unused and Reuse describe one set, not a change, so a difference
// leaves them zero.
func subtractModels(a, b Models) Models {
	pair := func(a, b *ModelPair) *ModelPair {
		return &ModelPair{Provisional: a.Provisional, Basis: a.Basis, Off: subtractResult(a.Off, b.Off), On: subtractResult(a.On, b.On)}
	}
	return Models{
		Overlay:  subtractResult(a.Overlay, b.Overlay),
		EROFS:    pair(a.EROFS, b.EROFS),
		CAS:      pair(a.CAS, b.CAS),
		LayerCAS: pair(a.LayerCAS, b.LayerCAS),
	}
}

// subtractResult returns a − b, unavailable for the first reason when
// either is.
func subtractResult(a, b *ModelResult) *ModelResult {
	switch {
	case !a.Available:
		return &ModelResult{Reason: a.Reason, Note: a.Note}
	case !b.Available:
		return &ModelResult{Reason: b.Reason, Note: a.Note}
	}
	return &ModelResult{Available: true, Note: a.Note, Breakdown: subtractBreakdown(a.Breakdown, b.Breakdown)}
}

// subtractBreakdown returns a − b over the additive fields and the counts.
func subtractBreakdown(a, b *Breakdown) *Breakdown {
	return &Breakdown{
		PayloadAlloc: a.PayloadAlloc - b.PayloadAlloc,
		Layout:       a.Layout - b.Layout,
		Verity:       a.Verity - b.Verity,
		OCIMeta:      a.OCIMeta - b.OCIMeta,
		Host:         a.Host - b.Host,
		Total:        a.Total - b.Total,
		PayloadBytes: a.PayloadBytes - b.PayloadBytes,
		Padding:      a.Padding - b.Padding,
		Inline:       a.Inline - b.Inline,
		ImagePad:     a.ImagePad - b.ImagePad,
		Units:        a.Units - b.Units,
		Objects:      a.Objects - b.Objects,
		Inodes:       a.Inodes - b.Inodes,
		Files:        a.Files - b.Files,
	}
}
