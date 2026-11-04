// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/pkg/version"
)

// sizeImages runs the images command and returns its JSON report.
func sizeImages(t testing.TB, open opener, args ...string) *Report {
	t.Helper()
	var out, errOut bytes.Buffer
	argv := append([]string{"images", "--json", "--platform", "linux/amd64"}, args...)
	if code := run(context.Background(), argv, strings.NewReader(""), &out, &errOut, open); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	return &rep
}

func baseLayers(t *testing.T) (a, b, c, d []byte) {
	a = tarOf(t, dir("./"), dir("usr/"), dir("etc/"), file("etc/os-release", strings.Repeat("o", 300)), file("usr/big", strings.Repeat("u", 20000)))
	b = tarOf(t, dir("usr/"), file("usr/lib", strings.Repeat("l", 9000)), file("usr/tiny", "t"))
	c = tarOf(t, file("app", strings.Repeat("c", 7000)), whiteout("usr/tiny"))
	d = tarOf(t, file("app", strings.Repeat("d", 7000)))
	return
}

func TestImagesReport(t *testing.T) {
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	rep := sizeImages(t, newSpyOpener().open, append([]string{"--no-cache"}, refs...)...)

	if len(rep.Images) != 2 {
		t.Fatalf("%d images", len(rep.Images))
	}
	res := results(rep)
	if len(res) != 7 {
		t.Fatalf("%d results", len(res))
	}
	for k, m := range res {
		if m == nil || !m.Available {
			t.Fatalf("model %s: %+v", k, m)
		}
		if rep.EstimatorVersions[strings.Split(k, "/")[0]] == "" {
			t.Errorf("no estimator version for %s", k)
		}
		host := int64(0)
		if rep.Settings.Profile.HostMetadata {
			host = m.Host
		}
		if m.PayloadAlloc+m.Layout+m.Verity+m.OCIMeta+host != m.Total || m.Total <= 0 {
			t.Errorf("%s: total %d does not reconstruct", k, m.Total)
		}
	}
	for _, k := range []string{"erofs", "cas", "layercas"} {
		if off, on := res[k+"/off"], res[k+"/on"]; off.Verity != 0 || on.Verity <= 0 || on.Total-off.Total != on.Verity {
			t.Errorf("%s: verity off %d/%d, on %d/%d", k, off.Verity, off.Total, on.Verity, on.Total)
		}
	}
	if lc := rep.Models.LayerCAS; !lc.Provisional || lc.Basis != layerCASBasis || !slices.ContainsFunc(rep.Pins, func(p string) bool { return strings.Contains(p, "ce384325d") }) {
		t.Errorf("layercas %+v", lc)
	}
	if res["overlay"].Units != 4 || res["cas/on"].Units != 2 {
		t.Errorf("snapshots %d, images %d", res["overlay"].Units, res["cas/on"].Units)
	}
	for _, img := range rep.Images {
		if img.Canonical == nil || !*img.Canonical || img.CASImage == "" {
			t.Errorf("image %s: canonical %v, CAS image %q", img.Ref, img.Canonical, img.CASImage)
		}
	}
}

func TestConfigOnlyChangeSharesCASImage(t *testing.T) {
	a, b, _, _ := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b}, label: "1"}, testImage{layers: [][]byte{a, b}, label: "2"})
	rep := sizeImages(t, newSpyOpener().open, append([]string{"--no-cache"}, refs...)...)
	if rep.Images[0].CASImage != rep.Images[1].CASImage || rep.Models.CAS.On.Units != 1 || rep.Models.Overlay.Units != 2 {
		t.Errorf("images %s %s, %d CAS images, %d snapshots", rep.Images[0].CASImage, rep.Images[1].CASImage, rep.Models.CAS.On.Units, rep.Models.Overlay.Units)
	}
}

func TestCASFollowsOcicfs(t *testing.T) {
	layer := tarOf(t, dir("./"), dir("usr/"), dir("run/"), file("run/big", strings.Repeat("r", 1000)),
		tent{name: "usr/x", body: strings.Repeat("x", 1000), xattrs: map[string]string{"user.k": "v"}})
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{layer}})
	rep := sizeImages(t, newSpyOpener().open, "--no-cache", refs[0])
	if cas := rep.Models.CAS.On; cas.Objects != 1 || cas.PayloadBytes != 1000 {
		t.Errorf("objects %d bytes %d: /run should be emptied", cas.Objects, cas.PayloadBytes)
	}
	if o := rep.Models.Overlay; o.PayloadBytes != 2000 {
		t.Errorf("overlay payload %d", o.PayloadBytes)
	}

	noUsr := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{tarOf(t, dir("./"), file("a", "a"))}})
	rep = sizeImages(t, newSpyOpener().open, "--no-cache", noUsr[0])
	if c := rep.Images[0].Canonical; c == nil || *c {
		t.Errorf("canonical %v without /usr", c)
	}
}

func TestUnavailableModels(t *testing.T) {
	l1 := tarOf(t, dir("./"), file("a", strings.Repeat("a", 5000)))
	l2 := tarOf(t, hardlink("b", "a"))
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{l1, l2}})
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"images", "--json", "--no-cache", "--platform", "linux/amd64", refs[0]}, nil, &out, &errOut, newSpyOpener().open); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var raw struct {
		Models map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	variant := func(k, v string) map[string]any { return raw.Models[k][v].(map[string]any) }
	for k, m := range map[string]map[string]any{"overlay": raw.Models["overlay"], "erofs": variant("erofs", "verityOff"), "layercas": variant("layercas", "verityOn")} {
		if m["available"] != false || !strings.Contains(m["reason"].(string), "cross-layer hardlink") {
			t.Errorf("%s: %v", k, m)
		}
		if _, ok := m["total"]; ok {
			t.Errorf("%s has a total", k)
		}
	}
	if cas := variant("cas", "verityOn"); cas["available"] != true || cas["objects"].(float64) != 1 {
		t.Errorf("cas: %v", cas)
	}

	// An ocicfs rejection disables only the CAS column.
	ctx := context.Background()
	sp, err := newSpool("")
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()
	ok := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{l1}})
	imgs, ls, err := loadImages(ctx, newSpyOpener().open, sp, "", ok, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	ls.failed[imgs[0].Layers[0].DiffID] = errors.New("boom")
	delete(ls.records, imgs[0].Layers[0].DiffID)
	rep := buildReport(ctx, imgs, ls, Settings{Profile: testProfile()})
	for k, m := range results(rep) {
		if want := !strings.HasPrefix(k, "cas/"); m.Available != want {
			t.Errorf("%s available %t", k, m.Available)
		}
	}
	if !strings.Contains(rep.Models.CAS.Off.Reason, "rejected by ocicfs") {
		t.Errorf("reason %q", rep.Models.CAS.Off.Reason)
	}
}

func TestLayerCASVerityOffIsLabelled(t *testing.T) {
	a, _, _, _ := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a}})
	rep := sizeImages(t, newSpyOpener().open, "--no-cache", refs[0])
	if lc := rep.Models.LayerCAS; lc.Off.Note != invalidLayerCAS || lc.On.Note != "" {
		t.Errorf("layercas off %q, on %q", lc.Off.Note, lc.On.Note)
	}
}

func TestCacheReuse(t *testing.T) {
	stampVersion(t, "v1.0.0", "0123abc")
	a, b, c, d := baseLayers(t)
	dir := t.TempDir()
	refs := writeLayout(t, dir, testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}},
		testImage{layers: [][]byte{a}, mediaType: ocispec.MediaTypeImageLayerGzip},
		testImage{layers: [][]byte{a}, mediaType: ocispec.MediaTypeImageLayerZstd})
	cache := filepath.Join(t.TempDir(), "cache")
	layerOpens := func(s *spyOpener) map[digest.Digest]int {
		out := map[digest.Digest]int{}
		for d, n := range s.opens {
			out[d] = n
		}
		return out
	}

	// One run over two images sharing a base reads each layer blob once
	// and scans each layer once per scanner.
	spy := newSpyOpener()
	rep := sizeImages(t, spy.open, "--cache-dir", cache, refs[0], refs[1])
	for dgst, n := range layerOpens(spy) {
		if n != 1 {
			t.Errorf("blob %s opened %d times", dgst, n)
		}
	}
	if an := rep.Analyzer; an.LayerScans != 4 || an.OcicfsScans != 4 || an.BlobsFetched != 4+4 {
		t.Errorf("first run: %+v", an)
	}

	// A second run fetches nothing.
	spy = newSpyOpener()
	again := sizeImages(t, spy.open, "--cache-dir", cache, refs[0], refs[1])
	if an := again.Analyzer; an.BlobsFetched != 0 || an.LayerScans != 0 || an.OcicfsScans != 0 || spy.total() != 0 {
		t.Errorf("second run: %+v, %d opens", an, spy.total())
	}
	cold := results(rep)
	for k, m := range results(again) {
		if m.Total != cold[k].Total {
			t.Errorf("%s: %d from cache, %d cold", k, m.Total, cold[k].Total)
		}
	}
	if again.Images[0].CASImage != rep.Images[0].CASImage {
		t.Error("CAS image identity differs between cached and cold layers")
	}

	// A layer recompressed under another blob digest hits both caches.
	gz := sizeImages(t, newSpyOpener().open, "--cache-dir", cache, refs[2])
	zst := sizeImages(t, newSpyOpener().open, "--cache-dir", cache, refs[3])
	if gz.Analyzer.LayerScans != 0 || zst.Analyzer.LayerScans != 0 || zst.Analyzer.OcicfsScans != 0 {
		t.Errorf("recompressed layer rescanned: %+v %+v", gz.Analyzer, zst.Analyzer)
	}

	// An unstamped or dirty build keeps no ocicfs records.
	for _, v := range []string{"dev", "v1.0.0-dirty"} {
		version.Version = v
		rep := sizeImages(t, newSpyOpener().open, "--cache-dir", cache, refs[0])
		if rep.Analyzer.OcicfsScans != 3 || rep.Analyzer.LayerScans != 0 {
			t.Errorf("version %s: %+v", v, rep.Analyzer)
		}
	}
}

func TestCacheNewLayerOnly(t *testing.T) {
	stampVersion(t, "v1.0.0", "0123abc")
	a, b, c, d := baseLayers(t)
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{a, b, c}}, testImage{layers: [][]byte{a, b, d}})
	cache := t.TempDir()
	sizeImages(t, newSpyOpener().open, "--cache-dir", cache, refs[0])
	spy := newSpyOpener()
	rep := sizeImages(t, spy.open, "--cache-dir", cache, refs[1])
	if an := rep.Analyzer; an.LayerScans != 1 || an.OcicfsScans != 1 {
		t.Errorf("analyzer %+v", an)
	}
	newBlob := digest.Digest(rep.Images[0].Layers[2].BlobDigest)
	if spy.opens[newBlob] != 1 || len(spy.opens) != 3 {
		t.Errorf("opens %v: want the manifest, the config and the new layer only", spy.opens)
	}
}

func TestUsageErrors(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "refs")
	if err := os.WriteFile(empty, []byte("# none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"images"}, {"images", "--no-cache", "@" + empty}, {"images", "--verity", "all", "x"}, {"images", "--fs", "xfs", "x"}, {"images", "--host-metadata", "no", "x"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), argv, nil, &out, &errOut, newSpyOpener().open); code != 2 || !strings.HasPrefix(errOut.String(), "ocicfs-size: ") {
			t.Errorf("%v: exit %d: %s", argv, code, errOut.String())
		}
	}
}

// total returns how many blob reads the opener served.
func (s *spyOpener) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.opens {
		n += c
	}
	return n
}

// results returns every model result by model and fs-verity variant.
func results(rep *Report) map[string]*ModelResult {
	ms := rep.Models
	return map[string]*ModelResult{
		"overlay":   ms.Overlay,
		"erofs/off": ms.EROFS.Off, "erofs/on": ms.EROFS.On,
		"cas/off": ms.CAS.Off, "cas/on": ms.CAS.On,
		"layercas/off": ms.LayerCAS.Off, "layercas/on": ms.LayerCAS.On,
	}
}

func TestInlineMaxFlag(t *testing.T) {
	layer := tarOf(t, dir("./"), dir("usr/"), file("usr/a", strings.Repeat("a", 64)), file("usr/b", strings.Repeat("b", 65)), file("usr/c", strings.Repeat("c", 5000)))
	refs := writeLayout(t, t.TempDir(), testImage{layers: [][]byte{layer}})
	for _, c := range []struct {
		arg     string
		objects int
	}{{"64", 2}, {"65", 1}, {"5000", 0}, {"5001", 0}, {"1048576", 0}} {
		rep := sizeImages(t, newSpyOpener().open, "--no-cache", "--inline-max", c.arg, refs[0])
		if got := rep.Models.CAS.Off.Objects; got != c.objects || rep.Models.CAS.On.Objects != c.objects {
			t.Errorf("--inline-max %s: %d/%d objects, want %d", c.arg, got, rep.Models.CAS.On.Objects, c.objects)
		}
		if fmt.Sprint(rep.Settings.Profile.InlineMax) != c.arg || rep.Settings.Profile.InlineMaxLayer != 0 {
			t.Errorf("--inline-max %s: settings %+v", c.arg, rep.Settings.Profile)
		}
		beyond := c.arg == "5001" || c.arg == "1048576"
		if (rep.Settings.InlineMaxNote == inlineBeyondCap) != beyond || slices.Contains(rep.Assumptions, "File CAS/image: "+inlineBeyondCap) != beyond {
			t.Errorf("--inline-max %s: note %q", c.arg, rep.Settings.InlineMaxNote)
		}
		if rep.Models.LayerCAS.On.Objects != 3 {
			t.Errorf("--inline-max %s: layer CAS has %d objects", c.arg, rep.Models.LayerCAS.On.Objects)
		}
	}
	if rep := sizeImages(t, newSpyOpener().open, "--no-cache", refs[0]); rep.Settings.Profile.InlineMax != 64 {
		t.Errorf("default inline max %d", rep.Settings.Profile.InlineMax)
	}
	for _, arg := range []string{"63", "1048577"} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{"images", "--no-cache", "--inline-max", arg, refs[0]}, nil, &out, &errOut, newSpyOpener().open); code != 2 || !strings.Contains(errOut.String(), "--inline-max") {
			t.Errorf("--inline-max %s: exit %d: %s", arg, code, errOut.String())
		}
	}
}
