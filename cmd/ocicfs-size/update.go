// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"

	"github.com/foundriesio/ocicfs/internal/cli"
)

// UpdateReport is an update from the from images to the to images: the
// models over the set before, over both sets held together and over the set
// after cleanup, and the differences the update makes. The differences
// leave Unused and Reuse zero.
type UpdateReport struct {
	From        []string `json:"from"`
	To          []string `json:"to"`
	Rollback    bool     `json:"rollback"`
	Before      Models   `json:"before"`      // the from images
	Union       Models   `json:"union"`       // the from and to images together
	After       Models   `json:"after"`       // the to images, and the from images with rollback
	Additional  Models   `json:"additional"`  // union − before
	Reclaimable Models   `json:"reclaimable"` // union − after
}

// runUpdate sizes the update o names.
func runUpdate(ctx context.Context, o options, stdin io.Reader, stdout io.Writer, open opener) error {
	if len(o.from) == 0 || len(o.to) == 0 {
		return &cli.UsageError{Err: errors.New("both --from and --to are required")}
	}
	s, platform, cacheDir, err := runSettings(o)
	if err != nil {
		return err
	}
	fromRefs, err := readRefs(o.from, stdin)
	if err != nil {
		return err
	}
	toRefs, err := readRefs(o.to, stdin)
	if err != nil {
		return err
	}
	sp, err := newSpool(metaDir(cacheDir))
	if err != nil {
		return err
	}
	defer sp.close()

	ls := newLayerSet(cacheDir)
	known := map[string]*Image{}
	from, err := loadImages(ctx, open, sp, ls, fromRefs, platform, rootKey, known)
	if err != nil {
		return err
	}
	to, err := loadImages(ctx, open, sp, ls, toRefs, platform, rootKey, known)
	if err != nil {
		return err
	}
	rep := buildUpdateReport(ctx, from, to, o.rollback, ls, s)
	return writeReport(stdout, rep, sp, o)
}

// buildUpdateReport sizes the sets before, during and after the update from
// from to to. Every image is prepared once, and the report's images, layers
// and models describe both sets together.
func buildUpdateReport(ctx context.Context, from, to []*Image, rollback bool, ls *layerSet, s Settings) *Report {
	sets := map[*Image][]string{}
	var union []*Image
	for _, set := range []struct {
		name string
		imgs []*Image
	}{{"from", from}, {"to", to}} {
		for _, img := range set.imgs {
			if sets[img] == nil {
				union = append(union, img)
			}
			sets[img] = append(sets[img], set.name)
		}
	}
	after := to
	if rollback {
		after = union
	}
	rep, roots, cas, casErr := prepareReport(ctx, union, ls, s)
	index := map[*Image]int{}
	for i, img := range union {
		index[img] = i
		rep.Images[i].Sets = sets[img]
	}
	models := func(imgs []*Image) Models {
		var rs []*root
		var cs []*casRoot
		for _, img := range imgs {
			rs, cs = append(rs, roots[index[img]]), append(cs, cas[index[img]])
		}
		return setModels(rs, cs, casErr, s.Profile)
	}
	up := &UpdateReport{Rollback: rollback, Before: models(from), Union: models(union), After: models(after)}
	for _, img := range from {
		up.From = append(up.From, img.Ref)
	}
	for _, img := range to {
		up.To = append(up.To, img.Ref)
	}
	up.Additional = subtractModels(up.Union, up.Before)
	up.Reclaimable = subtractModels(up.Union, up.After)
	rep.Models, rep.Update = up.Union, up
	return rep
}

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
