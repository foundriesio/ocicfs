// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"

	"github.com/foundriesio/ocicfs/pkg/version"
)

// Report is everything one run estimates, and what it assumed doing so.
type Report struct {
	Schema            int               `json:"schema"`
	Tool              string            `json:"tool"`
	ToolVersion       string            `json:"toolVersion"`
	EstimatorVersions map[string]string `json:"estimatorVersions"`
	Pins              []string          `json:"pins"`
	Settings          Settings          `json:"settings"`
	Images            []ImageReport     `json:"images"`
	Layers            []LayerReport     `json:"layers"`
	Models            Models            `json:"models"`
	Analyzer          Analyzer          `json:"analyzer"`
	Assumptions       []string          `json:"assumptions"`
	Exclusions        []string          `json:"exclusions"`
	Unsupported       []string          `json:"unsupported,omitempty"`
}

// Settings are the inputs every model shares.
type Settings struct {
	Profile  Profile `json:"profile"` // fs-verity is set per column, not here
	Platform string  `json:"platform"`
	FS       string  `json:"fs"`
}

// ImageReport is one retained image and the composefs image ocicfs would
// build for it.
type ImageReport struct {
	*Image
	CASImage  string `json:"casImage,omitempty"` // sha256 of the composefs dump
	Canonical *bool  `json:"canonical,omitempty"`
}

// LayerReport is one distinct layer of the retained images.
type LayerReport struct {
	DiffID      string   `json:"diffID"`
	TarSize     int64    `json:"tarSize"`
	Entries     int      `json:"entries"`
	Notes       []string `json:"notes,omitempty"`
	Unsupported string   `json:"unsupported,omitempty"`
}

// Models holds every model's estimates: overlay, which has no fs-verity,
// once, and each other model with fs-verity off and on.
type Models struct {
	Overlay  *ModelResult `json:"overlay"`
	EROFS    *ModelPair   `json:"erofs"`
	CAS      *ModelPair   `json:"cas"`
	LayerCAS *ModelPair   `json:"layercas"`
}

// ModelPair is one model sized with fs-verity off and on.
type ModelPair struct {
	Provisional bool         `json:"provisional,omitempty"`
	Basis       string       `json:"basis,omitempty"`
	Off         *ModelResult `json:"verityOff"`
	On          *ModelResult `json:"verityOn"`
}

// ModelResult is one model's estimate, or why it has none.
type ModelResult struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Note      string `json:"note,omitempty"`
	*Breakdown
}

// Analyzer is what the run itself read: the blobs it downloaded and the
// layers it found in its cache.
type Analyzer struct {
	BytesFetched int64 `json:"bytesFetched"`
	BlobsFetched int   `json:"blobsFetched"`
	CacheHits    int   `json:"cacheHits"`
	LayerScans   int   `json:"layerScans"`
	OcicfsScans  int   `json:"ocicfsScans"`
}

const (
	reportSchema    = 1
	layerCASBasis   = "prototype on containerd v2.4+ (9b17276ec)"
	invalidLayerCAS = "not a valid configuration of the prototype (requires verity=require)"
)

// buildReport replays every image, runs the four models over the set and
// collects the report.
func buildReport(ctx context.Context, imgs []*Image, ls *layerSet, s Settings) *Report {
	p := s.Profile
	rep := &Report{
		Schema:      reportSchema,
		Tool:        "ocicfs-size",
		ToolVersion: version.String(),
		EstimatorVersions: map[string]string{
			"overlay":  "1",
			"erofs":    "1",
			"cas":      "1",
			"layercas": "1",
			"verity":   "1",
			"ext4":     "1",
		},
		Pins: []string{
			"containerd v2.4.2, default configuration",
			"erofs-utils v1.9.4 (EROFS/layer: mkfs.erofs --tar=f --aufs -Enoinline_data -b4096)",
			"File CAS/layer prototype ce384325d (mkfs.erofs --tar=headerball --aufs -b4096)",
			"composefs-c 1.0.8 (mkcomposefs --min-version=1 --max-version=1)",
			"ocicfs " + version.String(),
		},
		Settings:    s,
		Assumptions: assumptions(),
		Exclusions:  exclusions(),
	}

	mats := map[string]*matLayer{}
	var roots []*root
	for _, img := range imgs {
		r := &root{img: img}
		for _, l := range img.Layers {
			m, ok := mats[l.DiffID]
			if !ok {
				inv := ls.inv[l.DiffID]
				m = materialize(inv)
				mats[l.DiffID] = m
				rep.Layers = append(rep.Layers, LayerReport{DiffID: inv.DiffID, TarSize: inv.TarSize, Entries: len(inv.Entries), Notes: inv.Notes, Unsupported: inv.Unsupported})
				if inv.Unsupported != "" {
					rep.Unsupported = append(rep.Unsupported, "layer "+inv.DiffID+": "+inv.Unsupported)
				}
			}
			r.mats = append(r.mats, m)
		}
		r.view = replay(r.mats)
		if r.view.unsupported != "" {
			rep.Unsupported = append(rep.Unsupported, img.Ref+": "+r.view.unsupported)
		}
		roots = append(roots, r)
	}

	result := func(b *Breakdown, why string) *ModelResult {
		if b == nil {
			return &ModelResult{Reason: why}
		}
		return &ModelResult{Available: true, Breakdown: b}
	}
	off, on := p, p
	off.Verity, on.Verity = false, true
	pair := func(f func([]*root, Profile) (*Breakdown, string)) *ModelPair {
		return &ModelPair{Off: result(f(roots, off)), On: result(f(roots, on))}
	}
	rep.Models.Overlay = result(overlaySize(roots, off))
	rep.Models.EROFS = pair(erofsSize)
	rep.Models.LayerCAS = pair(layerCasSize)
	rep.Models.LayerCAS.Provisional, rep.Models.LayerCAS.Basis = true, layerCASBasis
	rep.Models.LayerCAS.Off.Note = invalidLayerCAS

	var casRoots []*casRoot
	var casErr string
	for _, img := range imgs {
		ir := ImageReport{Image: img}
		cr, err := prepareCASRoot(ctx, ls, img, p.InlineMax)
		if err != nil {
			if casErr == "" {
				casErr = "rejected by ocicfs: " + img.Ref + ": " + err.Error()
			}
		} else {
			casRoots = append(casRoots, cr)
			ir.CASImage = "sha256:" + cr.id
			ir.Canonical = &cr.canonical
		}
		rep.Images = append(rep.Images, ir)
	}
	if casErr != "" {
		rep.Models.CAS = &ModelPair{Off: &ModelResult{Reason: casErr}, On: &ModelResult{Reason: casErr}}
		rep.Unsupported = append(rep.Unsupported, casErr)
	} else {
		rep.Models.CAS = &ModelPair{Off: result(casSize(casRoots, off), ""), On: result(casSize(casRoots, on), "")}
	}
	rep.Analyzer = Analyzer{CacheHits: ls.invHits + ls.recordHits, LayerScans: ls.scans, OcicfsScans: ls.ocicfsScans}
	return rep
}

func assumptions() []string {
	return []string{
		"estimates after cleanup: every retained snapshot, object and image is kept and nothing else",
		"metadata sizes are modelled from the writers' layout rules, not measured",
		"ext4: 4 KiB blocks, 256-byte inodes, metadata_csum, no inline_data or ea_inode; contiguous allocation",
		"ext4: hashed directories assumed 75% full; an inode's xattrs sit all in the inode or all in one shared block",
		"sparse files are counted dense",
		"File CAS/image: composefs images are told apart by their dump, so images that differ only in fields mkcomposefs normalises are counted twice",
		"File CAS/image: fs-verity view names are spread over shards like content digests",
		"File CAS/layer: inline threshold 0 (layer metadata cannot inline file data) and sealed objects, as the prototype does; it mounts with verity=require, so fs-verity off is not a configuration it runs",
	}
}

func exclusions() []string {
	return []string{
		"OCI content cache (compressed blobs) of containerd",
		"containerd metadata.db",
		"writable container layers and application data",
		"ocicfs saved-layer scan records",
		"extraction workspace and garbage-collection delay",
	}
}

// writeJSON writes the report as indented JSON.
func writeJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
