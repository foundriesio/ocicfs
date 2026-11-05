// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/composefs"
	ocimat "github.com/foundriesio/ocicfs/internal/materialize"
	"github.com/foundriesio/ocicfs/internal/rootfs"
	"github.com/foundriesio/ocicfs/internal/zstdchunked"
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
	Wire        Wire     `json:"wire"`

	details     []string     // per-layer download lines of a plain pull, for --details
	deltaLayers []deltaLayer // what a delta pull fetches of each layer it reads
}

// Wire is what the update downloads by each transfer path.
type Wire struct {
	Containerd  WirePath `json:"containerd"`
	Ocicfs      WirePath `json:"ocicfs"`
	OcicfsDelta WirePath `json:"ocicfsDelta"`
}

// WirePath is what one transfer path fetches: the compressed blobs and the
// metadata reads, every read counted.
type WirePath struct {
	Unavailable string `json:"unavailable,omitempty"` // why the path is not estimated
	Bytes       int64  `json:"bytes"`                 // metadata included
	Metadata    int64  `json:"metadata"`              // index, manifest and config bytes
	Reads       int    `json:"reads"`                 // metadata reads
	Blobs       int    `json:"blobs"`                 // whole-blob reads
	Held        int    `json:"held,omitempty"`        // to images the store already holds
	TOCs        int    `json:"tocs,omitempty"`        // delta: table of contents reads
	TOCBytes    int64  `json:"tocBytes,omitempty"`    // delta: their bytes, unusable tables included
	Ranges      int    `json:"ranges,omitempty"`      // delta: range requests issued
	Planned     int64  `json:"planned,omitempty"`     // delta: bytes the planned ranges span, gaps included
	Fallbacks   int    `json:"fallbacks,omitempty"`   // delta: layers read whole for too many or too large ranges
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
	ls.readTOCs = true
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

	up.Wire.Containerd = containerdPull(from, to)
	why := scanUnsupported(roots)
	if why == "" {
		why = casErr
	}
	if why != "" {
		up.Wire.Ocicfs.Unavailable = why
		up.Wire.OcicfsDelta.Unavailable = why
	} else {
		rootOf, casOf := map[*Image]*root{}, map[*Image]*casRoot{}
		for i, img := range union {
			rootOf[img], casOf[img] = roots[i], cas[i]
		}
		up.Wire.Ocicfs, up.details = ocicfsPull(from, to, rootOf, casOf)
		var err error
		if up.Wire.OcicfsDelta, up.deltaLayers, err = deltaPull(from, to, rootOf, casOf, ls); err != nil {
			up.Wire.OcicfsDelta = WirePath{Unavailable: err.Error()}
		}
	}
	rep.Assumptions = append(rep.Assumptions, wireAssumptions...)
	rep.Models, rep.Update = up.Union, up
	return rep
}

// The assumptions the download estimates rest on.
var wireAssumptions = []string{
	"containerd: stock unpacker (fetch-all off), content store holds the from images' blobs",
	"ocicfs rows: the from images were pulled by that row's path and collected (gc); store holds their referenced objects",
	"ocicfs pull: a layer is saved when read from its tar stream, never when built from its table",
	"delta: TOC extents from the published blob; registry serves ranges; HTTP overhead and redirects not counted (5–13 % measured)",
	"survivors from ocicfs-size's replay, which agrees with ocicfs's merge for the layers it accepts",
}

// containerdPull is what containerd's unpacker fetches for the to images
// with the from images' blobs and snapshots in place: each image's blobs
// from its first layer whose snapshot is absent, minus the blobs the content
// store holds, and the metadata blobs it lacks.
func containerdPull(from, to []*Image) WirePath {
	chains, blobs, meta := map[string]bool{}, map[string]bool{}, map[string]bool{}
	held := func(img *Image) {
		for _, l := range img.Layers {
			chains[l.ChainID], blobs[l.BlobDigest] = true, true
		}
		for _, m := range metadataBlobs(img) {
			meta[m.digest] = true
		}
	}
	for _, img := range from {
		held(img)
	}
	var w WirePath
	for _, img := range to {
		first := len(img.Layers)
		for j, l := range img.Layers {
			if !chains[l.ChainID] {
				first = j
				break
			}
		}
		for _, l := range img.Layers[first:] {
			if !blobs[l.BlobDigest] {
				blobs[l.BlobDigest] = true
				w.Bytes += l.BlobSize
				w.Blobs++
			}
		}
		for _, m := range metadataBlobs(img) {
			if !meta[m.digest] {
				w.Metadata += m.size
				w.Reads++
			}
		}
		held(img)
	}
	w.Bytes += w.Metadata
	return w
}

// metadataBlob is an image's index, manifest or config.
type metadataBlob struct {
	digest string
	size   int64
}

func metadataBlobs(img *Image) []metadataBlob {
	out := []metadataBlob{{img.ManifestDigest, img.ManifestSize}, {img.ConfigDigest, img.ConfigSize}}
	if img.IndexDigest != "" {
		out = append(out, metadataBlob{img.IndexDigest, img.IndexSize})
	}
	return out
}

// ocicfsState is what an ocicfs store holds across the pulls of one update:
// the images by identity, the metadata blobs by digest, the saved layers by
// blob, size and diff_id, and the objects by content digest.
type ocicfsState struct {
	held, metadata, saved, objects map[string]bool
}

// newOcicfsState is the store the from images leave once collected: the
// objects their merged roots reference, and the layers save keeps saved.
func newOcicfsState(from []*Image, cas map[*Image]*casRoot, save func(*Image, int) bool) *ocicfsState {
	st := &ocicfsState{held: map[string]bool{}, metadata: map[string]bool{}, saved: map[string]bool{}, objects: map[string]bool{}}
	for _, img := range from {
		st.held[identity(img)] = true
		st.metadata[rootKey(img)], st.metadata[img.ManifestDigest] = true, true
		for j, l := range img.Layers {
			if save(img, j) {
				st.saved[savedKey(l)] = true
			}
		}
		for _, e := range cas[img].entries {
			if e.Hardlink == "" && e.Mode.IsRegular() && e.Content == nil && e.Size > composefs.InlineContentMax {
				st.objects[e.Digest.String()] = true
			}
		}
	}
	return st
}

// identity is what ocicfs keys an image by: its platform manifest and
// platform.
func identity(img *Image) string { return img.ManifestDigest + " " + img.Platform }

// savedKey is what a saved layer is valid for.
func savedKey(l LayerRef) string { return fmt.Sprintf("%s %d %s", l.BlobDigest, l.BlobSize, l.DiffID) }

// resolve charges w the metadata reads of pulling img and reports whether
// the store holds the image already. A held image reads at most its root,
// when the store lacks that blob; any other reads its root twice, then its
// manifest when the root is an index, and its config.
func (st *ocicfsState) resolve(w *WirePath, img *Image) (held bool, detail string) {
	root := rootKey(img)
	rootSize := img.ManifestSize
	if img.IndexDigest != "" {
		rootSize = img.IndexSize
	}
	if st.held[identity(img)] {
		w.Held++
		if st.metadata[root] {
			return true, img.Ref + ": held: no source reads"
		}
		st.metadata[root] = true
		w.Metadata += rootSize
		w.Reads++
		return true, img.Ref + ": held: new root read"
	}
	w.Metadata += 2*rootSize + img.ConfigSize
	w.Reads += 3
	if img.IndexDigest != "" {
		w.Metadata += img.ManifestSize
		w.Reads++
	}
	st.held[identity(img)] = true
	st.metadata[root], st.metadata[img.ManifestDigest] = true, true
	return false, ""
}

// readLayer stores every file layer j of r carries, read whole, and
// reports whether that read its blob: an image reads each blob once.
func (st *ocicfsState) readLayer(read map[string]bool, r *root, j int) bool {
	for d := range layerObjects(r.mats[j].inv) {
		st.objects[d] = true
	}
	blob := r.img.Layers[j].BlobDigest
	if read[blob] {
		return false
	}
	read[blob] = true
	return true
}

// layerObjects are the contents of the regular files of a layer ocicfs
// stores as objects, hidden or not.
func layerObjects(inv *LayerInv) map[string]bool {
	out := map[string]bool{}
	for _, e := range inv.Entries {
		if e.Kind == KindReg && e.Size > composefs.InlineContentMax {
			out["sha256:"+e.Digest] = true
		}
	}
	return out
}

// survivor is a regular file an image shows: its content and the layer
// that supplies it.
type survivor struct {
	digest string
	size   int64
	layer  int
}

// survivors are the non-empty regular files r shows, one per content and
// supplying layer, before ocicfs's canonical transform.
func survivors(r *root) []survivor {
	seen := map[survivor]bool{}
	var out []survivor
	for li, m := range r.mats {
		for i := range m.names {
			e := m.entry(i)
			if r.view.fate[li][i] != fateVisible || e == nil || e.Kind != KindReg || e.Size == 0 {
				continue
			}
			s := survivor{digest: "sha256:" + e.Digest, size: e.Size, layer: li}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// ocicfsPull is what plain ocicfs pulls of the to images fetch into a store
// the from images were pulled into, every layer saved, and collected. Each
// image reads every layer it has no saved layer for, then each saved layer
// that is the first to list a stored file the image shows and the store
// still lacks. It returns the per-layer lines for --details too.
func ocicfsPull(from, to []*Image, roots map[*Image]*root, cas map[*Image]*casRoot) (WirePath, []string) {
	st := newOcicfsState(from, cas, func(*Image, int) bool { return true })
	var w WirePath
	var details []string
	for _, img := range to {
		if held, detail := st.resolve(&w, img); held {
			details = append(details, "ocicfs pull · "+detail)
			continue
		}
		r := roots[img]
		read := map[string]bool{}
		var reused []int
		for j, l := range img.Layers {
			if st.saved[savedKey(l)] {
				reused = append(reused, j)
				continue
			}
			if st.readLayer(read, r, j) {
				details = append(details, "ocicfs pull · "+l.BlobDigest+": read whole · not saved")
				w.Bytes += l.BlobSize
				w.Blobs++
			}
			st.saved[savedKey(l)] = true
		}
		missing := map[string]bool{}
		for _, s := range survivors(r) {
			if s.size > composefs.InlineContentMax && !st.objects[s.digest] {
				missing[s.digest] = true
			}
		}
		for _, j := range reused {
			if len(missing) == 0 {
				break
			}
			lists := false
			for d := range layerObjects(r.mats[j].inv) {
				if missing[d] {
					delete(missing, d)
					lists = true
				}
			}
			if lists && st.readLayer(read, r, j) {
				details = append(details, "ocicfs pull · "+img.Layers[j].BlobDigest+": read whole · recovery: lists a needed file the store lacks")
				w.Bytes += img.Layers[j].BlobSize
				w.Blobs++
			}
		}
	}
	w.Bytes += w.Metadata
	return w, details
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

// deltaLayer is what a delta pull fetches of one layer: its table of
// contents, the missing files it supplies, the ranges they take, and how
// the layer's bytes arrive.
type deltaLayer struct {
	blob             string
	size, tocBytes   int64
	table            bool
	missing, inline  int
	requests         int
	planned, fetched int64
	reason           string
}

// tableOf is the table of contents a delta pull reads for layer l: nil
// when the layer has none or it is unusable, with the bytes reading it
// costs and why the layer then goes to its tar stream.
func tableOf(ls *layerSet, l LayerRef) (*LayerTOC, int64, string) {
	pos, ok, err := zstdchunked.Position(layerDesc(l).Annotations, l.BlobSize)
	switch {
	case err != nil:
		return nil, 0, "toc-error"
	case !ok:
		return nil, 0, "no-toc"
	}
	t := ls.tocs[tocKey(l)]
	if t == nil || t.Error != "" {
		return nil, pos.CompressedLen, "toc-error"
	}
	return t, pos.CompressedLen, ""
}

// deltaPull is what delta pulls of the to images fetch into a store the
// from images were delta-pulled into and collected, so that only their
// layers without a usable table are saved. Each image reads every table,
// takes each other layer from its saved layer or reads it whole, and plans
// the files it shows and the store lacks as ranges, as the pull's own
// planner does; a stored file no table lists comes from the first saved
// layer listing it, read whole.
func deltaPull(from, to []*Image, roots map[*Image]*root, cas map[*Image]*casRoot, ls *layerSet) (WirePath, []deltaLayer, error) {
	st := newOcicfsState(from, cas, func(img *Image, j int) bool {
		t, _, _ := tableOf(ls, img.Layers[j])
		return t == nil
	})
	var w WirePath
	var out []deltaLayer
	for _, img := range to {
		if held, detail := st.resolve(&w, img); held {
			out = append(out, deltaLayer{reason: detail})
			continue
		}
		r := roots[img]
		read := map[string]bool{}
		layers := make([]deltaLayer, len(img.Layers))
		plan := make([]ocimat.PlanLayer, len(img.Layers))
		var reused []int
		for j, l := range img.Layers {
			t, tocBytes, reason := tableOf(ls, l)
			d := deltaLayer{blob: l.BlobDigest, size: l.BlobSize, tocBytes: tocBytes, fetched: tocBytes, reason: reason, table: t != nil}
			if tocBytes > 0 {
				w.TOCs++
				w.TOCBytes += tocBytes
			}
			plan[j].Size = l.BlobSize
			switch {
			case t != nil:
				d.reason = "ranges"
				for _, f := range t.Files {
					plan[j].Entries = append(plan[j].Entries, zstdchunked.Entry{Digest: digest.Digest(f.Digest), Size: f.Size, Offset: f.Offset, EndOffset: f.EndOffset})
				}
			case st.saved[savedKey(l)]:
				d.reason = "saved"
				reused = append(reused, j)
			default:
				st.readLayer(read, r, j)
				d.fetched += l.BlobSize
				w.Blobs++
				st.saved[savedKey(l)] = true
			}
			layers[j] = d
		}

		var missing []rootfs.Payload
		seen := map[string]bool{}
		for _, s := range survivors(r) {
			inline := s.size <= composefs.InlineContentMax
			if seen[s.digest] || (inline && !layers[s.layer].table) || (!inline && st.objects[s.digest]) {
				continue
			}
			seen[s.digest] = true
			missing = append(missing, rootfs.Payload{Digest: digest.Digest(s.digest), Size: s.size})
		}
		listed, unlisted := missing, []rootfs.Payload(nil)
		if len(reused) > 0 {
			listed, unlisted = ocimat.SplitUnlisted(plan, missing)
		}
		plans, err := ocimat.PlanRanges(plan, listed)
		if err != nil {
			return WirePath{}, nil, fmt.Errorf("%s: %w", img.Ref, err)
		}
		for _, p := range missing {
			st.objects[p.Digest.String()] = true
		}
		// An inline file comes from the lowest layer whose table lists it.
		for _, p := range listed {
			if p.Size > composefs.InlineContentMax {
				continue
			}
		lowest:
			for j := range plan {
				for _, e := range plan[j].Entries {
					if e.Digest == p.Digest && e.Size == p.Size {
						layers[j].inline++
						break lowest
					}
				}
			}
		}
		for _, p := range unlisted {
			for _, j := range reused {
				if layerObjects(r.mats[j].inv)[p.Digest.String()] {
					if layers[j].reason == "saved" {
						layers[j].reason = "saved, read whole"
						layers[j].fetched += img.Layers[j].BlobSize
						w.Blobs++
						st.readLayer(read, r, j)
					}
					layers[j].missing++
					break
				}
			}
		}
		for j, p := range plans {
			d := &layers[j]
			d.missing += p.Files
			d.requests = p.Requests
			d.planned = p.Planned
			w.Planned += p.Planned
			if p.Files == 0 {
				continue
			}
			if p.Fallback != "" {
				d.reason = p.Fallback
				d.fetched += d.size
				w.Blobs++
				w.Fallbacks++
				continue
			}
			d.fetched += p.Planned
			w.Ranges += p.Requests
		}
		for _, d := range layers {
			w.Bytes += d.fetched
		}
		out = append(out, layers...)
	}
	w.Bytes += w.Metadata
	return w, out, nil
}
