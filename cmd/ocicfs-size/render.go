// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// writeText writes the report for a reader: what each model takes with
// fs-verity off and on, and with details everything the run assumed and
// read.
func writeText(w io.Writer, rep *Report, details bool) error {
	var b strings.Builder
	writeCompact(&b, rep)
	if details {
		writeDetails(&b, rep)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeCompact writes the header, the images' OCI layers, each model's
// disk usage with fs-verity off and on, each against overlay, and the host
// profile in the heading.
func writeCompact(b *strings.Builder, rep *Report) {
	s, p := rep.Settings, rep.Settings.Profile
	fmt.Fprintf(b, "STORAGE ESTIMATE · %s · %s\n", plural(len(rep.Images), "image"), s.Platform)
	refs := 0
	var total, unique int64
	blobs := map[string]bool{}
	for _, img := range rep.Images {
		ref := img.Ref
		if !strings.Contains(ref, "@") {
			ref += "@" + img.ManifestDigest
		}
		fmt.Fprintf(b, "- %s\n", ref)
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

	ms := rep.Models
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
		if !on.Available {
			rows = append(rows, []string{r.name, "n/a‡", "—", "n/a‡", "—", "n/a‡", "—"})
			continue
		}
		onCell := fmtSize(on.Total)
		if off.Total > 0 {
			onCell += " (" + signedPercent(on.Total-off.Total, off.Total) + ")"
		}
		rows = append(rows, []string{r.name, fmtInt(on.Files), vsFiles(on), fmtSize(off.Total), vs(off), onCell, vs(on)})
	}
	heading := fmt.Sprintf("Estimated disk usage (formula-based): %s · %d KiB blocks · no compression · inline max size %s B (File CAS/image)", s.FS, p.HostBlock/1024, fmtInt(p.InlineMax))
	if !p.HostMetadata {
		heading += " · host metadata excluded"
	}
	writeModelTable(b, heading, rows)

	var reasons []string
	unavailable := map[string][]string{}
	for _, m := range []struct {
		name string
		r    *ModelResult
	}{{"Overlay", overlay}, {"EROFS/layer", ms.EROFS.On}, {"File CAS/image", ms.CAS.On}, {"File CAS/layer", ms.LayerCAS.On}} {
		if !m.r.Available {
			if unavailable[m.r.Reason] == nil {
				reasons = append(reasons, m.r.Reason)
			}
			unavailable[m.r.Reason] = append(unavailable[m.r.Reason], m.name)
		}
	}
	for _, why := range reasons {
		fmt.Fprintf(b, "‡ %s unavailable: %s\n", strings.Join(unavailable[why], ", "), why)
	}
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
