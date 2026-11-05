// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// writeText writes the report for a reader: what each model takes with
// fs-verity off and on, and with details what each model's store keeps and
// everything the run assumed and read.
func writeText(w io.Writer, rep *Report, details bool) error {
	var b strings.Builder
	if rep.Update != nil {
		writeUpdate(&b, rep, details)
	} else {
		writeCompact(&b, rep)
		writeModelTable(&b, diskHeading("Estimated disk usage", rep.Settings), modelRows(rep.Models))
		writeUnavailable(&b, unavailableModels(rep.Models))
	}
	if details {
		writeProjection(&b, rep)
		writeDetails(&b, rep)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeCompact writes the header and the images' OCI layers.
func writeCompact(b *strings.Builder, rep *Report) {
	s := rep.Settings
	fmt.Fprintf(b, "STORAGE ESTIMATE · %s · %s\n", plural(len(rep.Images), "image"), s.Platform)
	refs := 0
	var total, unique int64
	blobs := map[string]bool{}
	for _, img := range rep.Images {
		fmt.Fprintf(b, "- %s\n", pinnedRef(img))
		for _, l := range img.Layers {
			refs++
			total += l.BlobSize
			if !blobs[l.BlobDigest] {
				blobs[l.BlobDigest] = true
				unique += l.BlobSize
			}
		}
	}
	fmt.Fprintf(b, "\nOCI layers:       %s · %s\n", plural(refs, "reference"), plural(len(blobs), "unique blob"))
	fmt.Fprintf(b, "Compressed size:  %s total · %s unique\n\n", fmtSize(total), fmtSize(unique))
}

// modelRows are the rows of a model table: each model's stored files and
// size with fs-verity off and on, each against overlay.
func modelRows(ms Models) [][]string {
	overlay := ms.Overlay
	vs := func(m *ModelResult) string {
		if !overlay.Available || overlay.Total == 0 {
			return "—"
		}
		d := m.Total - overlay.Total
		return fmt.Sprintf("%s (%s)", signed(fmtSize(d)), signedPercent(d, overlay.Total))
	}
	vsFiles := func(m *ModelResult) string {
		if !overlay.Available || overlay.Files == 0 {
			return "—"
		}
		d := m.Files - overlay.Files
		return fmt.Sprintf("%s (%s)", signed(fmtInt(d)), signedPercent(d, overlay.Files))
	}
	rows := [][]string{}
	if overlay.Available {
		rows = append(rows, []string{"Overlay", fmtInt(overlay.Files), "—", fmtSize(overlay.Total), "—", "—", "—"})
	} else {
		rows = append(rows, []string{"Overlay", "n/a‡", "—", "n/a‡", "—", "—", "—"})
	}
	for _, r := range []struct {
		name string
		mp   *ModelPair
	}{{"EROFS/layer", ms.EROFS}, {"File CAS/image", ms.CAS}, {"File CAS/layer", ms.LayerCAS}} {
		off, on := r.mp.Off, r.mp.On
		if !on.Available || !off.Available {
			rows = append(rows, []string{r.name, "n/a‡", "—", "n/a‡", "—", "n/a‡", "—"})
			continue
		}
		onCell := fmtSize(on.Total)
		if off.Total != 0 {
			onCell += " (" + signedPercent(on.Total-off.Total, off.Total) + ")"
		}
		rows = append(rows, []string{r.name, fmtInt(on.Files), vsFiles(on), fmtSize(off.Total), vs(off), onCell, vs(on)})
	}
	return rows
}

// diskHeading is a disk table's heading: what it estimates, then the host
// profile.
func diskHeading(what string, s Settings) string {
	return what + " (formula-based): " + hostProfile(s)
}

// hostProfile names the host filesystem the disk figures are sized for.
func hostProfile(s Settings) string {
	p := s.Profile
	profile := fmt.Sprintf("%s · %d KiB blocks · no compression · inline max size %s B (File CAS/image)", s.FS, p.HostBlock/1024, fmtInt(p.InlineMax))
	if !p.HostMetadata {
		profile += " · host metadata excluded"
	}
	return profile
}

// modelNames are the storage models in table order.
var modelNames = [4]string{"Overlay", "EROFS/layer", "File CAS/image", "File CAS/layer"}

// verityOn is each model's result with fs-verity on, in table order;
// Overlay has none and gives its only result.
func verityOn(ms Models) [4]*ModelResult {
	return [4]*ModelResult{ms.Overlay, ms.EROFS.On, ms.CAS.On, ms.LayerCAS.On}
}

// unavailable is a row a run could not estimate and why.
type unavailable struct{ name, reason string }

// unavailableModels lists the models ms has no estimate for.
func unavailableModels(ms Models) []unavailable {
	var out []unavailable
	for i, r := range verityOn(ms) {
		if !r.Available {
			out = append(out, unavailable{modelNames[i], r.Reason})
		}
	}
	return out
}

// writeUnavailable writes one footnote per reason, naming the rows it
// leaves without an estimate.
func writeUnavailable(b *strings.Builder, rows []unavailable) {
	var reasons []string
	names := map[string][]string{}
	for _, r := range rows {
		if names[r.reason] == nil {
			reasons = append(reasons, r.reason)
		}
		names[r.reason] = append(names[r.reason], r.name)
	}
	for _, why := range reasons {
		fmt.Fprintf(b, "‡ %s unavailable: %s\n", strings.Join(names[why], ", "), why)
	}
}

// writeUpdate writes an update's header, what each transfer path
// downloads, and each model's disk usage before the update, needed while
// both sets are held and after cleanup, with fs-verity on and off. A model
// one of the sets cannot be estimated for is unavailable in every column.
// With details the sets' OCI layers, each path's requests and its per-layer
// reasons follow.
func writeUpdate(b *strings.Builder, rep *Report, details bool) {
	from, to, after := updateSets(rep)
	writeUpdateHeader(b, rep, from, to)
	writeDownload(b, rep.Update.Wire, from, to)
	writeDiskTables(b, rep)
	if details {
		writeUpdateLayers(b, rep, from, after)
		up := rep.Update
		fmt.Fprintf(b, "containerd pull: %s\nocicfs pull: %s\n", requests(up.Wire.Containerd), requests(up.Wire.Ocicfs))
		writeList(b, "Download details", up.details)
	}
}

// updateSets are the images an update names with --from and --to, and the
// images held after cleanup.
func updateSets(rep *Report) (from, to, after []ImageReport) {
	for _, img := range rep.Images {
		for _, set := range img.Sets {
			if set == "from" {
				from = append(from, img)
			} else {
				to = append(to, img)
			}
		}
	}
	if rep.Update.Rollback {
		return from, to, rep.Images
	}
	return from, to, to
}

// writeUpdateHeader names the update: one image to another by name, or
// the sizes of the two sets and every image of them.
func writeUpdateHeader(b *strings.Builder, rep *Report, from, to []ImageReport) {
	platform := rep.Settings.Platform
	if len(from) == 1 && len(to) == 1 {
		f, t := from[0], to[0]
		after := imageName(t) + " only"
		if rep.Update.Rollback {
			after = imageName(t) + " + " + imageName(f) + " kept"
		}
		fmt.Fprintf(b, "UPDATE ESTIMATE · %s → %s · %s · after cleanup: %s\n", imageName(f), imageName(t), platform, after)
		fmt.Fprintf(b, "- from: %s\n- to:   %s\n", shortRef(f), shortRef(t))
		return
	}
	after, nAfter := "to only", len(to)
	if rep.Update.Rollback {
		after, nAfter = "to + from kept", len(rep.Images)
	}
	fmt.Fprintf(b, "UPDATE ESTIMATE · %d → %s · %s · after cleanup: %s\n", len(from), plural(nAfter, "image"), platform, after)
	for _, img := range from {
		fmt.Fprintf(b, "- from: %s\n", pinnedRef(img))
	}
	for _, img := range to {
		fmt.Fprintf(b, "- to:   %s\n", pinnedRef(img))
	}
}

// imageName is an image's short name: its familiar name and tag, or the
// name and short digest of a reference without a tag.
func imageName(img ImageReport) string {
	ref, _, _ := strings.Cut(img.Ref, "@")
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return path.Base(ref) + "@" + shortDigest(img.ManifestDigest)
	}
	if _, ok := named.(reference.Tagged); ok {
		return reference.FamiliarString(named)
	}
	return reference.FamiliarName(named) + "@" + shortDigest(img.ManifestDigest)
}

// shortRef is an image's reference and the short digest of its manifest.
func shortRef(img ImageReport) string {
	ref, _, _ := strings.Cut(img.Ref, "@")
	return ref + " @" + shortDigest(img.ManifestDigest)
}

// shortDigest is the first 12 hex digits of a digest.
func shortDigest(d string) string {
	hex := digest.Digest(d).Encoded()
	return hex[:min(12, len(hex))]
}

// writeDownload writes one row per transfer path: what it downloads, the
// layers it fetches of the to images', and the storage models it feeds.
func writeDownload(b *strings.Builder, wire Wire, from, to []ImageReport) {
	_, _, blobsFrom := imageBlobs(from)
	_, _, blobsTo := imageBlobs(to)
	of := fmt.Sprintf(" of %s", plural(len(blobsTo), "layer"))
	since := "the from set"
	if len(from) == 1 {
		since = imageName(from[0])
	}
	shared := "none"
	if n := sharedBlobs(blobsFrom, blobsTo); n > 0 {
		shared = fmtInt(int64(n))
	}
	rows := [][]string{{"Transfer path", "Download", "What it fetches", "Feeds"}}
	var gone []unavailable
	for _, p := range []struct {
		name, fetches, feeds string
		w                    WirePath
	}{
		{"containerd pull", fmt.Sprintf(" (%s shared with %s)", shared, since), "every storage model", wire.Containerd},
		{"ocicfs pull", "", "File CAS/image", wire.Ocicfs},
	} {
		if p.w.Unavailable != "" {
			rows = append(rows, []string{p.name, "n/a‡", "-", p.feeds})
			gone = append(gone, unavailable{p.name, p.w.Unavailable})
			continue
		}
		rows = append(rows, []string{p.name, fmtSize(p.w.Bytes), fmtInt(int64(p.w.Blobs)) + of + p.fetches, p.feeds})
	}
	b.WriteString("\n")
	writeTable(b, "Download (compressed bytes the device fetches):", rows, []int{3, 2, 2}, []bool{false, true, false, false})
	writeUnavailable(b, gone)
}

// requests counts the requests a transfer path takes, by kind.
func requests(w WirePath) string {
	var parts []string
	if w.Blobs > 0 {
		parts = append(parts, plural(w.Blobs, "blob"))
	}
	parts = append(parts, fmtInt(int64(w.Reads))+" metadata")
	return strings.Join(parts, " + ")
}

// writeDiskTables writes each model's disk usage before, needed during and
// after the update, with fs-verity on and then off, and what the columns
// and the figures assume.
func writeDiskTables(b *strings.Builder, rep *Report) {
	up := rep.Update
	for _, on := range []bool{true, false} {
		heading := "Disk usage (fs-verity off):"
		if on {
			heading = "Disk usage (fs-verity on; Overlay has none):"
		}
		b.WriteString("\n")
		writeTable(b, heading, diskRows(up.Before, up.Additional, up.After, on), diskTableGaps[:], []bool{false, true, true, true, true, true})
	}
	writeUnavailable(b, unavailableModels(up.Union))
	kept := "after = to images only"
	if up.Rollback {
		kept = "after = to images, from images kept"
	}
	fmt.Fprintf(b, "\nbefore = from images held · needed = extra space while both are held (rollback window) · %s\n", kept)
	fmt.Fprintf(b, "%s · writable layers, blob cache and app data excluded\n", hostProfile(rep.Settings))
}

// The disk table's gaps between its columns.
var diskTableGaps = [5]int{3, 3, 2, 2, 2}

// diskRows are a disk table's titles and one row per model, with fs-verity
// on or off: the usage before, the space needed and against Overlay's, and
// the usage after and against Overlay's.
func diskRows(before, needed, after Models, on bool) [][]string {
	pick := func(ms Models) []*ModelResult {
		if on {
			return []*ModelResult{ms.Overlay, ms.EROFS.On, ms.CAS.On, ms.LayerCAS.On}
		}
		return []*ModelResult{ms.Overlay, ms.EROFS.Off, ms.CAS.Off, ms.LayerCAS.Off}
	}
	bs, ns, as := pick(before), pick(needed), pick(after)
	vs := func(m, overlay *ModelResult) string {
		if m == overlay || !overlay.Available || overlay.Total == 0 {
			return "-"
		}
		d := m.Total - overlay.Total
		return fmt.Sprintf("%s (%s)", signed(fmtSize(d)), signedPercent(d, overlay.Total))
	}
	rows := [][]string{{"Storage model", "before", "needed", "vs Overlay", "after", "vs Overlay"}}
	for i, name := range modelNames {
		bf, nd, af := bs[i], ns[i], as[i]
		if !bf.Available || !nd.Available || !af.Available {
			rows = append(rows, []string{name, "n/a‡", "n/a‡", "-", "n/a‡", "-"})
			continue
		}
		rows = append(rows, []string{name, fmtSize(bf.Total), signed(fmtSize(nd.Total)), vs(nd, ns[0]), fmtSize(af.Total), vs(af, as[0])})
	}
	alignParens(rows[1:], utf8.RuneCountInString, func(n int) string { return strings.Repeat(" ", max(n, 0)) })
	return rows
}

// writeTable writes rows under heading, the first row the column titles,
// with a dashed rule under the heading and under the titles: the columns
// gaps apart, each right-aligned where right says so and left-aligned
// otherwise.
func writeTable(b *strings.Builder, heading string, rows [][]string, gaps []int, right []bool) {
	width := utf8.RuneCountInString
	pad := func(n int) string { return strings.Repeat(" ", max(n, 0)) }
	w := make([]int, len(rows[0]))
	for _, r := range rows {
		for j, c := range r {
			w[j] = max(w[j], width(c))
		}
	}
	total := w[0]
	for j := 1; j < len(w); j++ {
		total += gaps[j-1] + w[j]
	}
	rule := strings.Repeat("─", max(total, width(heading)))
	line := func(s string) { b.WriteString(strings.TrimRight(s, " ") + "\n") }
	line(heading)
	line(rule)
	for i, r := range rows {
		l := r[0] + pad(w[0]-width(r[0]))
		for j := 1; j < len(r); j++ {
			if right[j] {
				l += pad(gaps[j-1]) + pad(w[j]-width(r[j])) + r[j]
			} else {
				l += pad(gaps[j-1]) + r[j] + pad(w[j]-width(r[j]))
			}
		}
		line(l)
		if i == 0 {
			line(rule)
		}
	}
}

// writeUpdateLayers writes the OCI layers and the compressed size of the
// two sets.
func writeUpdateLayers(b *strings.Builder, rep *Report, from, after []ImageReport) {
	nA, sizeA, blobsA := imageBlobs(from)
	nB, sizeB, blobsB := imageBlobs(after)
	_, _, all := imageBlobs(rep.Images)
	fmt.Fprintf(b, "\nOCI layers:       %d → %s · %s · %d shared\n", nA, plural(nB, "reference"), plural(len(all), "unique blob"), sharedBlobs(blobsA, blobsB))
	fmt.Fprintf(b, "Compressed size:  %s → %s\n", fmtSize(sizeA), fmtSize(sizeB))
}

// imageBlobs counts the layer references of imgs, the size of their
// distinct blobs, and the blobs.
func imageBlobs(imgs []ImageReport) (refs int, size int64, blobs map[string]bool) {
	blobs = map[string]bool{}
	for _, img := range imgs {
		for _, l := range img.Layers {
			refs++
			if !blobs[l.BlobDigest] {
				blobs[l.BlobDigest] = true
				size += l.BlobSize
			}
		}
	}
	return refs, size, blobs
}

// sharedBlobs counts the blobs of b that a holds.
func sharedBlobs(a, b map[string]bool) int {
	n := 0
	for d := range b {
		if a[d] {
			n++
		}
	}
	return n
}

// pinnedRef is an image's reference with the digest it resolved to.
func pinnedRef(img ImageReport) string {
	if strings.Contains(img.Ref, "@") {
		return img.Ref
	}
	return img.Ref + "@" + img.ManifestDigest
}

// alignParens lines up, column by column, the cells of rows that read
// "value (percentage)" on their opening parenthesis: the values right-
// aligned before it, the percentages left-aligned after it.
func alignParens(rows [][]string, width func(string) int, pad func(int) string) {
	for j := 1; j < len(rows[0]); j++ {
		var left, right int
		for _, r := range rows {
			if a, c, ok := strings.Cut(r[j], " ("); ok {
				left, right = max(left, width(a)), max(right, width(c))
			}
		}
		for _, r := range rows {
			if a, c, ok := strings.Cut(r[j], " ("); ok {
				r[j] = pad(left-width(a)) + a + " (" + c + pad(right-width(c))
			}
		}
	}
}

// The model table's gaps: after the model name, between the two cells of
// the file count group, between it and the fs-verity off group, between
// the two cells of that group, between the fs-verity groups, and between
// the two cells of the fs-verity on group.
var modelTableGaps = [6]int{3, 2, 5, 2, 5, 2}

// writeModelTable writes rows of a model name and six right-aligned cells,
// file count and against overlay, size and against overlay with fs-verity
// off, then the same with it on, under a heading, the two fs-verity group
// headings and column titles, with a dashed rule under the heading and
// under the titles.
func writeModelTable(b *strings.Builder, heading string, rows [][]string) {
	titles := []string{"Storage model", "Stored files", "vs Overlay", "Size", "vs Overlay", "Size (vs own off)", "vs Overlay off"}
	width := utf8.RuneCountInString
	pad := func(n int) string { return strings.Repeat(" ", max(n, 0)) }
	alignParens(rows, width, pad)
	w := make([]int, len(titles))
	for _, r := range append([][]string{titles}, rows...) {
		for j, c := range r {
			w[j] = max(w[j], width(c))
		}
	}
	center := func(t string, n int) string { return pad((n-width(t))/2) + t }
	g := modelTableGaps
	files := w[1] + g[1] + w[2]
	off := w[3] + g[3] + w[4]
	on := w[5] + g[5] + w[6]
	total := w[0] + g[0] + files + g[2] + off + g[4] + on
	line := func(s string) { b.WriteString(strings.TrimRight(s, " ") + "\n") }
	total = max(total, width(heading))
	rule := strings.Repeat("─", total)
	line(heading)
	line(rule)
	offHead := center("fs-verity off", off)
	line(pad(w[0]+g[0]+files+g[2]) + offHead + pad(off-width(offHead)+g[4]) + center("fs-verity on", on))
	for i, r := range append([][]string{titles}, rows...) {
		l := r[0] + pad(w[0]-width(r[0]))
		for j := 1; j < len(r); j++ {
			l += pad(g[j-1]) + pad(w[j]-width(r[j])) + r[j]
		}
		line(l)
		if i == 0 {
			line(rule)
		}
	}
}

// The storage projection's geometry: every box is boxWidth runes wide, and
// the largest total fills barCells bar cells.
const (
	boxWidth = 72
	barCells = 40
)

// writeProjection draws one box per model: the allocation its store keeps
// as a bar on a scale all boxes share, the components of that allocation,
// its reuse and how the store is mounted.
func writeProjection(b *strings.Builder, rep *Report) {
	ms := rep.Models
	var largest int64
	for _, m := range []*ModelResult{ms.Overlay, ms.EROFS.On, ms.CAS.On, ms.LayerCAS.On} {
		if m.Available {
			largest = max(largest, m.Total)
		}
	}
	cells := func(n int64) int {
		if largest == 0 {
			return 0
		}
		return int((2*barCells*n + largest) / (2 * largest))
	}
	bar := func(off, on *ModelResult) string {
		return barLine(cells(off.Total), cells(on.Total)-cells(off.Total), on.Total)
	}
	host := func(br *Breakdown) string {
		if !rep.Settings.Profile.HostMetadata {
			return "host metadata excluded"
		}
		return "host " + fmtMiB(br.Host) + " MiB"
	}
	unused := func(br *Breakdown) string {
		return fmt.Sprintf("unused versions %s (%s MiB allocated, kept)", fmtInt(int64(br.Unused.Versions)), fmtMiB(br.Unused.Alloc))
	}
	// reuseLines are a box's reuse lines, one metric each: the stored units
	// shared between images, the contents used more than once, as named,
	// and the contents kept in several copies, as named.
	reuseLines := func(r *Reuse, units, used, usedBytes, duplicated string) []string {
		if r == nil {
			return nil
		}
		dup := "  duplicated " + duplicated + ": none"
		if r.Duplicated > 0 {
			dup = fmt.Sprintf("  duplicated %s %s (%s extra copies, %s MiB)",
				duplicated, fmtInt(int64(r.Duplicated)), fmtInt(int64(r.ExtraCopies)), fmtMiB(r.ExtraBytes))
		}
		return []string{
			fmt.Sprintf("reuse: %s shared between images %s of %s (%s MiB)",
				units, fmtInt(int64(r.UnitsReused)), fmtInt(int64(r.UnitsReferenced)), fmtMiB(r.UnitBytes)),
			fmt.Sprintf("  %s %s (%s MiB %s)", used, fmtInt(int64(r.Contents)), fmtMiB(r.ContentBytes), usedBytes),
			dup,
		}
	}
	box := func(title string, lines, reuse []string, mount string) {
		writeBox(b, title, append(append(lines, reuse...), mount))
	}

	b.WriteString("\nSTORAGE PROJECTION · modelled allocation per store, not paths\n")
	fmt.Fprintf(b, "█ allocation without fs-verity · ▒ fs-verity increment\n1 cell ≈ %s MiB (%d cells = largest total)\n", fmtMiB(largest/barCells), barCells)

	if o := ms.Overlay; !o.Available {
		writeUnavailableBox(b, "Overlay")
	} else {
		box("Overlay · "+fmtMiB(o.Total)+" MiB", []string{
			nouns(o.Units, "upper directory", "upper directories") + ", one per snapshot",
			bar(o, o),
			fmt.Sprintf("file payload %s MiB (of which padding %s MiB)", fmtMiB(o.PayloadAlloc), fmtMiB(o.Padding)),
			host(o.Breakdown) + " · " + plural(int(o.Inodes), "inode"),
			unused(o.Breakdown),
		}, reuseLines(o.Reuse, "snapshots", "hardlinked contents", "named more than once", "contents"),
			"→ overlayfs mount over the snapshot chain; no stored rootfs copy")
	}

	if off, on := ms.EROFS.Off, ms.EROFS.On; !on.Available {
		writeUnavailableBox(b, "EROFS/layer")
	} else {
		box("EROFS/layer · "+fmtMiB(on.Total)+" MiB with fs-verity", []string{
			plural(on.Units, "layer.erofs image") + ", one per snapshot",
			bar(off, on),
			fmt.Sprintf("without fs-verity %s MiB · fs-verity +%s MiB", fmtMiB(off.Total), fmtMiB(on.Verity)),
			fmt.Sprintf("file payload %s MiB · image layout %s MiB · %s", fmtMiB(on.PayloadAlloc), fmtMiB(on.Layout), host(on.Breakdown)),
			unused(on.Breakdown),
		}, reuseLines(on.Reuse, "snapshots", "hardlinked contents", "named more than once", "contents"),
			"→ overlayfs over per-layer EROFS mounts; no stored rootfs copy")
	}

	if off, on := ms.CAS.Off, ms.CAS.On; !on.Available {
		writeUnavailableBox(b, "File CAS/image")
	} else {
		box("File CAS/image · "+fmtMiB(on.Total)+" MiB with fs-verity", []string{
			fmt.Sprintf("%s (%s MiB) · %s (%s MiB)", plural(on.Units, "composefs image"), fmtMiB(on.Layout), plural(on.Objects, "shared object"), fmtMiB(on.PayloadAlloc)),
			bar(off, on),
			fmt.Sprintf("without fs-verity %s MiB", fmtMiB(off.Total)),
			fmt.Sprintf("fs-verity +%s MiB · OCI metadata %s MiB · %s", fmtMiB(on.Verity), fmtMiB(on.OCIMeta), host(on.Breakdown)),
			fmt.Sprintf("inline content %s MiB, inside the composefs images", fmtMiB(on.Inline)),
		}, reuseLines(on.Reuse, "rootfs images", "contents used more than once", "of extra uses", "inline contents"),
			"→ composefs mount: metadata image + objects; no stored rootfs copy")
	}

	if off, on := ms.LayerCAS.Off, ms.LayerCAS.On; !on.Available {
		writeUnavailableBox(b, "File CAS/layer")
	} else {
		box("File CAS/layer · "+fmtMiB(on.Total)+" MiB with fs-verity", []string{
			fmt.Sprintf("%s (%s MiB) · %s (%s MiB)", plural(on.Units, "layer metadata image"), fmtMiB(on.Layout), plural(on.Objects, "object"), fmtMiB(on.PayloadAlloc)),
			bar(off, on),
			fmt.Sprintf("without fs-verity %s MiB (not a valid configuration)", fmtMiB(off.Total)),
			fmt.Sprintf("fs-verity +%s MiB · %s · %s", fmtMiB(on.Verity), host(on.Breakdown), unused(on.Breakdown)),
		}, reuseLines(on.Reuse, "snapshots", "objects used more than once", "of extra uses", "contents"),
			"→ one flat overlay: layer metadata + objects as data-only lower")
	}

	b.WriteString("\nReuse is logical bytes, before block rounding and fs-verity. Every name of a\n" +
		"content is a use; hardlinked names share one stored copy. Snapshot and\n" +
		"file-level reuse overlap and are never added.\n" +
		"Unused = invisible in every retained final rootfs; counted in disk usage,\n" +
		"not necessarily reclaimable.\n")
}

// writeDetails writes what the run assumed, pinned and read, and every
// image's resolved identities.
func writeDetails(b *strings.Builder, rep *Report) {
	p := rep.Settings.Profile
	snaps := map[string]bool{}
	for _, img := range rep.Images {
		for _, l := range img.Layers {
			snaps[l.ChainID] = true
		}
	}
	writeList(b, "Notes", []string{
		plural(len(rep.Layers), "layer") + " · " + plural(len(snaps), "snapshot"),
		fmt.Sprintf("EROFS %d KiB blocks", p.ErofsBlock/1024),
		fmt.Sprintf("File CAS inline: image ≤%s B · layer %s B (fixed)", fmtInt(p.InlineMax), fmtInt(p.InlineMaxLayer)),
		"File CAS/layer: " + layerCASBasis + ", not a release; 'off' is " + invalidLayerCAS,
		"after cleanup; writable layers and application data excluded",
	})
	writeList(b, "Assumptions", rep.Assumptions)
	writeList(b, "Exclusions", rep.Exclusions)
	writeList(b, "Pins", rep.Pins)
	writeList(b, "Estimator versions", sortedPairs(rep.EstimatorVersions))
	writeList(b, "Unsupported", rep.Unsupported)
	var notes []string
	for _, l := range rep.Layers {
		for _, n := range l.Notes {
			notes = append(notes, l.DiffID+": "+n)
		}
	}
	writeList(b, "Layer notes", notes)
	var images []string
	for _, img := range rep.Images {
		s := img.Ref + ":"
		if img.IndexDigest != "" {
			s += " index " + img.IndexDigest + ","
		}
		s += " manifest " + img.ManifestDigest
		if img.Canonical != nil {
			s += fmt.Sprintf(", composefs image %s, canonical %t", img.CASImage, *img.Canonical)
		}
		images = append(images, s)
	}
	writeList(b, "Images", images)
	a := rep.Analyzer
	fmt.Fprintf(b, "\nanalyzer: fetched %s MiB (%s); %s; %s, %s\n",
		fmtMiB(a.BytesFetched), plural(a.BlobsFetched, "blob"), plural(a.CacheHits, "cache hit"),
		plural(a.LayerScans, "layer scan"), plural(a.OcicfsScans, "ocicfs scan"))
}

// writeBox draws a box of boxWidth runes around lines, wrapping at spaces
// each line too long to fit.
func writeBox(b *strings.Builder, title string, lines []string) {
	fmt.Fprintf(b, "\n┌─ %s %s┐\n", title, strings.Repeat("─", max(0, boxWidth-5-utf8.RuneCountInString(title))))
	for _, l := range lines {
		ws := []string{l}
		if utf8.RuneCountInString(l) > boxWidth-4 {
			ws = wrap(l, boxWidth-4)
		}
		for _, w := range ws {
			fmt.Fprintf(b, "│ %s%s │\n", w, strings.Repeat(" ", max(0, boxWidth-4-utf8.RuneCountInString(w))))
		}
	}
	b.WriteString("└" + strings.Repeat("─", boxWidth-2) + "┘\n")
}

// writeUnavailableBox draws the box of a model the run could not size.
func writeUnavailableBox(b *strings.Builder, name string) {
	writeBox(b, name+" · n/a‡", []string{"unavailable: see ‡ above"})
}

// barLine is a bar of base solid and verity shaded cells, with total in
// MiB right-aligned in a box line.
func barLine(base, verity int, total int64) string {
	bar := strings.Repeat("█", base) + strings.Repeat("▒", verity)
	num := fmtMiB(total) + " MiB"
	return bar + strings.Repeat(" ", max(1, boxWidth-4-utf8.RuneCountInString(bar)-utf8.RuneCountInString(num))) + num
}

// wrap breaks s at spaces into lines of at most width runes, indenting
// every line after the first by two spaces. A word longer than width
// stays whole.
func wrap(s string, width int) []string {
	var out []string
	line := ""
	for _, word := range strings.Split(s, " ") {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
			line += " " + word
		default:
			out = append(out, line)
			line = "  " + word
		}
	}
	return append(out, line)
}

func writeList(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, it := range items {
		fmt.Fprintf(b, "  - %s\n", it)
	}
}

func sortedPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+": "+v)
	}
	sort.Strings(out)
	return out
}

// fmtMiB formats n bytes in MiB with one decimal and thousands separators.
func fmtMiB(n int64) string {
	return groupThousands(fmt.Sprintf("%.1f", float64(n)/(1<<20)))
}

// fmtSize formats n bytes in MiB from 1 MiB up and in KiB from 1 KiB, with
// one decimal, and in whole bytes below. Sizes stay in MiB past 1 GiB so
// that differences of a few MiB show.
func fmtSize(n int64) string {
	switch abs := max(n, -n); {
	case abs >= 1<<20:
		return fmtMiB(n) + " MiB"
	case abs >= 1<<10:
		return fmt.Sprintf("%.1f", float64(n)/(1<<10)) + " KiB"
	default:
		return fmtInt(n) + " B"
	}
}

// fmtInt formats n with thousands separators.
func fmtInt(n int64) string {
	return groupThousands(fmt.Sprint(n))
}

// groupThousands puts a comma between every three digits of the integer
// part of a decimal number.
func groupThousands(s string) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	frac := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s, frac = s[:i], s[i:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s + frac
}

// signed gives a number without a minus sign a plus sign.
func signed(s string) string {
	if strings.HasPrefix(s, "-") {
		return s
	}
	return "+" + s
}

// signedPercent is n as a signed percentage of base, with one decimal.
// A share that rounds to 100.0% without being all of base takes two
// decimals, so that a remainder does not read as none.
func signedPercent(n, base int64) string {
	pct := 100 * float64(n) / float64(base)
	s := fmt.Sprintf("%.1f", pct)
	if (s == "100.0" || s == "-100.0") && n != base && n != -base {
		s = fmt.Sprintf("%.2f", pct)
	}
	return signed(s) + "%"
}

// plural counts n of a noun that takes an s in the plural.
func plural(n int, noun string) string {
	return nouns(n, noun, noun+"s")
}

// nouns counts n of a noun, one or many.
func nouns(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmtInt(int64(n)) + " " + many
}
