// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/zstdchunked"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
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

// blobSize is the size of a blob of the layout at dir.
func blobSize(t *testing.T, dir, d string) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, "blobs", "sha256", digest.Digest(d).Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestWireRollback checks the from images --rollback keeps are not
// pulled again.
func TestWireRollback(t *testing.T) {
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	plain := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1]).Update.Wire
	kept := sizeUpdate(t, newSpyOpener().open, "--rollback", "--from", refs[0], "--to", refs[1]).Update.Wire
	if kept.Containerd != plain.Containerd || kept.Ocicfs != plain.Ocicfs || kept.Ocicfs.Held != 0 {
		t.Errorf("rollback %+v, without %+v", kept, plain)
	}
	if text := runText(t, "update", "--details", "--no-cache", "--rollback", "--from", refs[0], "--to", refs[1]); strings.Contains(text, ": held:") {
		t.Errorf("rollback pulls a kept image:\n%s", text)
	}
}

func TestWireUnchanged(t *testing.T) {
	a, b, c, _ := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}})
	w := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[0]).Update.Wire
	if w.Containerd != (WirePath{}) {
		t.Errorf("containerd %+v", w.Containerd)
	}
	if w.Ocicfs != (WirePath{Held: 1}) {
		t.Errorf("ocicfs %+v", w.Ocicfs)
	}
}

func TestWireConfigOnly(t *testing.T) {
	a, b, _, _ := baseLayers(t)
	dir := t.TempDir()
	refs := writeLayout(t, dir, testImage{layers: [][]byte{a, b}, label: "1"}, testImage{layers: [][]byte{a, b}, label: "2"})
	rep := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1])
	to := rep.Images[1]
	manifest, config := blobSize(t, dir, to.ManifestDigest), blobSize(t, dir, to.ConfigDigest)
	w := rep.Update.Wire
	if want := (WirePath{Bytes: manifest + config, Metadata: manifest + config, Reads: 2}); w.Containerd != want {
		t.Errorf("containerd %+v, want %+v", w.Containerd, want)
	}
	// Every layer is saved, so only the metadata is read: the root twice.
	meta := 2*manifest + config
	if want := (WirePath{Bytes: meta, Metadata: meta, Reads: 3}); w.Ocicfs != want {
		t.Errorf("ocicfs %+v, want %+v", w.Ocicfs, want)
	}
}

func TestWireContainerdPin(t *testing.T) {
	a, b, _, d := baseLayers(t)
	x := tarOf(t, file("x", strings.Repeat("x", 3000)))
	gz := ocispec.MediaTypeImageLayerGzip
	from := testImage{layers: [][]byte{a, b}}
	for _, c := range []struct {
		name  string
		to    testImage
		blobs int
	}{
		// a recompressed below the first new snapshot: the snapshot is there.
		{"recompressed below", testImage{layers: [][]byte{a, b, d}, layerType: []string{gz}}, 1},
		// b recompressed above it: extracted again, so its new blob is fetched.
		{"recompressed above", testImage{layers: [][]byte{a, x, b}, layerType: []string{"", "", gz}}, 2},
		// b unchanged above it: its blob is in the content store.
		{"shared above", testImage{layers: [][]byte{a, x, b}}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			refs := writeLayout(t, t.TempDir(), from, c.to)
			if w := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1]).Update.Wire.Containerd; w.Blobs != c.blobs {
				t.Errorf("%d blobs, want %d", w.Blobs, c.blobs)
			}
		})
	}
}

func TestWireSavedLayers(t *testing.T) {
	big := strings.Repeat("X", 5000)
	l0 := tarOf(t, dir("./"), file("x", big))
	l1 := tarOf(t, file("w", big))
	gone := tarOf(t, whiteout("x"), whiteout("w"))
	carry := tarOf(t, file("y", big))
	gz := ocispec.MediaTypeImageLayerGzip
	for _, c := range []struct {
		name   string
		from   testImage
		to     testImage
		blobs  []int // the to layers read whole
		detail string
	}{
		// The store lost x's content with the from image, and a layer the
		// to image reads anyway carries it again.
		{"carried by an unsaved layer", testImage{layers: [][]byte{l0, gone}}, testImage{layers: [][]byte{l0, carry}}, []int{1}, "not saved"},
		// No unsaved layer carries it: the first saved layer listing it is
		// read whole, once.
		{"recovered", testImage{layers: [][]byte{l0, l1, gone}}, testImage{layers: [][]byte{l0, l1}}, []int{0}, "recovery"},
		// A recompressed layer has no saved layer.
		{"recompressed", testImage{layers: [][]byte{l0}}, testImage{layers: [][]byte{l0}, layerType: []string{gz}}, []int{0}, "not saved"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			refs := writeLayout(t, dir, c.from, c.to)
			rep := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1])
			to := rep.Images[len(rep.Images)-1]
			var want int64
			for _, j := range c.blobs {
				want += blobSize(t, dir, to.Layers[j].BlobDigest)
			}
			w := rep.Update.Wire.Ocicfs
			if w.Blobs != len(c.blobs) || w.Bytes-w.Metadata != want {
				t.Errorf("%d blobs of %d bytes, want %d of %d", w.Blobs, w.Bytes-w.Metadata, len(c.blobs), want)
			}
			text := runText(t, "update", "--no-cache", "--details", "--from", refs[0], "--to", refs[1])
			if !strings.Contains(text, to.Layers[c.blobs[0]].BlobDigest+": read whole · "+c.detail) {
				t.Errorf("no %q detail line:\n%s", c.detail, text)
			}
		})
	}
}

func TestWireUnsupportedReplay(t *testing.T) {
	a, _, _, _ := baseLayers(t)
	l1 := tarOf(t, dir("./"), file("a", strings.Repeat("a", 5000)))
	l2 := tarOf(t, hardlink("b", "a"))
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a}}, testImage{layers: [][]byte{l1, l2}})
	up := sizeUpdate(t, newSpyOpener().open, "--from", refs[0], "--to", refs[1]).Update
	if w := up.Wire; w.Containerd.Unavailable != "" || w.Containerd.Blobs != 2 || w.Ocicfs.Unavailable == "" {
		t.Errorf("wire %+v", w)
	}
	if !up.Additional.CAS.On.Available || up.Additional.Overlay.Available {
		t.Errorf("cas %+v, overlay %+v", up.Additional.CAS.On, up.Additional.Overlay)
	}
	text := runText(t, "update", "--no-cache", "--from", refs[0], "--to", refs[1])
	if !strings.Contains(text, "‡ ocicfs pull unavailable: ") {
		t.Errorf("no footnote for the ocicfs row:\n%s", text)
	}
}

// runText runs a command and returns its text output.
func runText(t *testing.T, argv ...string) string {
	t.Helper()
	argv = append([]string{argv[0], "--platform", "linux/amd64"}, argv[1:]...)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), argv, nil, &out, &errOut, newSpyOpener().open); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	return out.String()
}

// writeIndex writes an image index of the manifests refs name into the
// layout at dir, the first for linux/amd64 and the rest for linux/arm64,
// and returns a reference to it.
func writeIndex(t *testing.T, dir string, refs ...string) string {
	t.Helper()
	idx := ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex}
	for i, ref := range refs {
		d := ref[strings.LastIndex(ref, "@")+1:]
		p := &ocispec.Platform{OS: "linux", Architecture: "amd64"}
		if i > 0 {
			p = &ocispec.Platform{OS: "linux", Architecture: "arm64"}
		}
		idx.Manifests = append(idx.Manifests, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(d), Size: blobSize(t, dir, d), Platform: p})
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	d := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", d.Encoded()), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir + "@" + d.String()
}

// TestWireMetadataReads checks the ocicfs row's reads against ocicfs
// pulling the from image, collecting, and pulling the to image.
func TestWireMetadataReads(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the layouts are linux/amd64")
	}
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	a, b, c, d := baseLayers(t)
	dir := t.TempDir()
	m := writeLayout(t, dir, testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, d}}, testImage{layers: [][]byte{b, d}})
	i1 := writeIndex(t, dir, m[0], m[1])
	i2 := writeIndex(t, dir, m[0], m[2]) // the same amd64 image under a new index
	for _, c := range []struct {
		name, from, to string
		reads          int
	}{
		{"held pinned image", m[0], m[0], 0},
		{"held manifest under a new index", i1, i2, 1},
		{"index to its selected manifest", i1, m[0], 0},
		{"unheld index", m[1], i1, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, cleanup, err := devtest.VerityRoot()
			if err != nil {
				t.Skip(err)
			}
			defer cleanup()
			spy := newSpyOpener()
			e, err := ocicfs.New(ocicfs.Config{Root: root, RegistrySource: func(ctx context.Context, ref string) (ocicfs.BlobSource, digest.Digest, error) {
				return spy.open(ctx, dir+ref[strings.LastIndex(ref, "@"):])
			}})
			if err != nil {
				t.Fatal(err)
			}
			pull := func(ref string) {
				t.Helper()
				if _, err := e.Pull(context.Background(), "example.test/app"+ref[strings.LastIndex(ref, "@"):]); err != nil {
					if devtest.IsVerityUnsupported(err) {
						t.Skip(err)
					}
					t.Fatalf("pull %s: %v", ref, err)
				}
			}
			pull(c.from)
			if _, err := e.GC(context.Background()); err != nil {
				t.Fatal(err)
			}
			spy.opens = map[digest.Digest]int{}
			pull(c.to)

			rep := sizeUpdate(t, newSpyOpener().open, "--from", c.from, "--to", c.to)
			layers := map[digest.Digest]bool{}
			for _, img := range rep.Images {
				for _, l := range img.Layers {
					layers[digest.Digest(l.BlobDigest)] = true
				}
			}
			var reads, blobs int
			var bytes int64
			for d, n := range spy.opens {
				if layers[d] {
					blobs += n
				} else {
					reads += n
					bytes += int64(n) * blobSize(t, dir, d.String())
				}
			}
			w := rep.Update.Wire.Ocicfs
			if reads != c.reads || w.Reads != reads || w.Metadata != bytes || w.Blobs != blobs {
				t.Errorf("ocicfs read %d metadata blobs, %d bytes, %d layers; estimated %d, %d, %d; want %d reads", reads, bytes, blobs, w.Reads, w.Metadata, w.Blobs, c.reads)
			}
		})
	}
}

func TestUpdateNewIndex(t *testing.T) {
	a, b, c, d := baseLayers(t)
	dir := t.TempDir()
	m := writeLayout(t, dir, testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, d}}, testImage{layers: [][]byte{b, d}})
	rep := sizeUpdate(t, newSpyOpener().open, "--from", writeIndex(t, dir, m[0], m[1]), "--to", writeIndex(t, dir, m[0], m[2]))
	if len(rep.Images) != 2 || fmt.Sprint(rep.Images[0].Sets, rep.Images[1].Sets) != "[from] [to]" {
		t.Fatalf("images %+v", rep.Images)
	}
	add := modelResults(rep.Update.Additional)
	for k, m := range add {
		if !strings.HasPrefix(k, "cas/") && (m.Total != 0 || m.Files != 0) {
			t.Errorf("%s: +%d bytes, +%d files", k, m.Total, m.Files)
		}
	}
	// One index object, and the binding the estimator keys by root.
	if cas := add["cas/off"]; cas.OCIMeta != 4096 || cas.PayloadAlloc != 0 || cas.Units != 0 {
		t.Errorf("cas %+v", cas.Breakdown)
	}
}

// The zstd:chunked release pair: two layers each, the base layer shared.
var (
	releaseN  = filepath.Join("..", "..", "internal", "zstdchunked", "testdata", "release-n") + "@sha256:f73453797a21f103d2a7cbc3e1ff649e6da0cd84d70b388e705c0cd8f45c3418"
	releaseN1 = filepath.Join("..", "..", "internal", "zstdchunked", "testdata", "release-n1") + "@sha256:ae2b0c5dc46df52b06d4bbf5978b4086ef10da994512ecef2dc7d158d4c1d46d"
)

// layerOpens counts the whole layer blobs spy read.
func layerOpens(rep *Report, spy *spyOpener) (opens, ranges int) {
	seen := map[string]bool{}
	for _, img := range rep.Images {
		for _, l := range img.Layers {
			if !seen[l.BlobDigest] {
				seen[l.BlobDigest] = true
				opens += spy.opens[digest.Digest(l.BlobDigest)]
			}
		}
	}
	for _, n := range spy.ranges {
		ranges += n
	}
	return opens, ranges
}

func TestTOCCache(t *testing.T) {
	stampVersion(t, "v1.0.0", "0123abc")
	cache := t.TempDir()
	update := func(args ...string) (*Report, int, int) {
		spy := newSpyOpener()
		rep := sizeUpdateCached(t, spy.open, append([]string{"--cache-dir", cache}, args...)...)
		opens, ranges := layerOpens(rep, spy)
		return rep, opens, ranges
	}
	// The first run reads every blob whole for its inventory, so the
	// tables come out of the spool.
	rep, opens, ranges := update("--from", releaseN, "--to", releaseN1)
	if a := rep.Analyzer; opens != 3 || ranges != 0 || a.TOCParses != 3 || a.TOCRanges != 0 || a.TOCHits != 0 {
		t.Errorf("first run: %d opens, %d ranges, analyzer %+v", opens, ranges, a)
	}
	rep, opens, ranges = update("--from", releaseN, "--to", releaseN1)
	if a := rep.Analyzer; opens != 0 || ranges != 0 || a.TOCHits != 3 || a.TOCParses != 0 || a.BlobsFetched != 0 {
		t.Errorf("second run: %d opens, %d ranges, analyzer %+v", opens, ranges, a)
	}

	// A cache without tables reads each one as a range, not its blob.
	if err := os.RemoveAll(filepath.Join(cache, "tocs")); err != nil {
		t.Fatal(err)
	}
	rep, opens, ranges = update("--from", releaseN, "--to", releaseN1)
	if a := rep.Analyzer; opens != 0 || ranges != 3 || a.TOCParses != 3 || a.TOCRanges != 3 {
		t.Errorf("without tables: %d opens, %d ranges, analyzer %+v", opens, ranges, a)
	}

	// The same blob under other annotations is a miss.
	other := relocatedTOC(t, releaseN1)
	rep, opens, ranges = update("--from", releaseN, "--to", other)
	if a := rep.Analyzer; opens != 0 || ranges != 1 || a.TOCParses != 1 || a.TOCHits != 2 {
		t.Errorf("other annotations: %d opens, %d ranges, analyzer %+v", opens, ranges, a)
	}
}

// TestTOCAfterImages checks an update after images sized both sets reads
// no blob whole, only the tables.
func TestTOCAfterImages(t *testing.T) {
	stampVersion(t, "v1.0.0", "0123abc")
	cache := t.TempDir()
	sizeImages(t, newSpyOpener().open, "--cache-dir", cache, releaseN, releaseN1)
	spy := newSpyOpener()
	rep := sizeUpdateCached(t, spy.open, "--cache-dir", cache, "--from", releaseN, "--to", releaseN1)
	if opens, ranges := layerOpens(rep, spy); opens != 0 || ranges != 3 || rep.Analyzer.TOCRanges != 3 {
		t.Errorf("%d opens, %d ranges, analyzer %+v", opens, ranges, rep.Analyzer)
	}
	text := runText(t, "update", "--details", "--cache-dir", cache, "--platform", "linux/amd64", "--from", releaseN, "--to", releaseN1)
	if !strings.Contains(text, "; 3 tables of contents (0 parsed, 0 ranged, 3 hits)") {
		t.Errorf("no table counts in the analyzer line:\n%s", text)
	}
}

// relocatedTOC copies the layout ref names into a new directory with its
// last layer's position annotation spelled differently, and returns a
// reference to the copy's manifest.
func relocatedTOC(t *testing.T, ref string) string {
	t.Helper()
	i := strings.LastIndex(ref, "@")
	src, dst := ref[:i], t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "blobs", "sha256", digest.Digest(ref[i+1:]).Encoded())) // #nosec G304 -- a test layout
	if err != nil {
		t.Fatal(err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	last := m.Layers[len(m.Layers)-1]
	last.Annotations[zstdchunked.ManifestPositionKey] = "0" + last.Annotations[zstdchunked.ManifestPositionKey]
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	d := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(dst, "blobs", "sha256", d.Encoded()), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst + "@" + d.String()
}

// sizeUpdateCached is sizeUpdate with the caller's cache flags.
func sizeUpdateCached(t testing.TB, open opener, args ...string) *Report {
	t.Helper()
	var out, errOut bytes.Buffer
	argv := append([]string{"update", "--json", "--platform", "linux/amd64"}, args...)
	if code := run(context.Background(), argv, strings.NewReader(""), &out, &errOut, open); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	return &rep
}
