// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// goldenImages are the image sets the text output is checked against.
func goldenImages(t *testing.T) map[string][]testImage {
	a, b, c, d := baseLayers(t)
	return map[string][]testImage{
		"one-image":  {{layers: [][]byte{a, b, c, tarOf(t, dir("usr/"), file("usr/data", strings.Repeat("x", 3<<20)))}}},
		"two-images": {{layers: [][]byte{a, b, c}}, {layers: [][]byte{a, b, d}}},
		"unavailable": {{layers: [][]byte{
			tarOf(t, dir("./"), file("a", strings.Repeat("a", 5000))),
			tarOf(t, hardlink("b", "a")),
		}}},
	}
}

func TestTextGolden(t *testing.T) {
	stampVersion(t, "v1.0.0", "0123abc")
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatal(err)
	}
	for name, images := range goldenImages(t) {
		for _, mode := range []string{"default", "details"} {
			t.Run(mode+"-"+name, func(t *testing.T) {
				// A fixed layout path keeps the references, and so how File
				// CAS/image shards their reference files, the same every run.
				t.Chdir(t.TempDir())
				refs := writeLayout(t, "LAYOUT", images...)
				argv := append([]string{"images", "--no-cache", "--platform", "linux/amd64"}, refs...)
				if mode == "details" {
					argv = append(argv, "--details")
				}
				var out, errOut bytes.Buffer
				if code := run(context.Background(), argv, nil, &out, &errOut, newSpyOpener().open); code != 0 {
					t.Fatalf("exit %d: %s", code, errOut.String())
				}
				checkWidths(t, out.String())
				golden(t, filepath.Join(goldenDir, mode+"-"+name+".txt"), out.String())
			})
		}
	}
}

func TestNumberFormats(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{fmtMiB(0), "0.0"},
		{fmtMiB(1234567890123), "1,177,375.7"},
		{fmtMiB(1 << 30), "1,024.0"},
		{fmtMiB(-40000), "-0.0"},
		{fmtMiB(-1234 << 20), "-1,234.0"},
		{fmtInt(999), "999"},
		{fmtInt(1000), "1,000"},
		{fmtInt(-1234567), "-1,234,567"},
		{plural(1, "blob"), "1 blob"},
		{plural(34414, "object"), "34,414 objects"},
		{fmtSize(1023), "1,023 B"},
		{fmtSize(-4 << 10), "-4.0 KiB"},
		{fmtSize(3421 << 20), "3,421.0 MiB"},
		{signedPercent(-180810, 180848), "-99.98%"},
		{signedPercent(-180848, 180848), "-100.0%"},
		{signedPercent(1, 8), "+12.5%"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// checkWidths checks the storage projection: every box line is boxWidth
// runes wide and no other line exceeds 80 runes.
func checkWidths(t *testing.T, out string) {
	t.Helper()
	_, proj, _ := strings.Cut(out, "\nSTORAGE PROJECTION")
	proj, _, _ = strings.Cut(proj, "\nNotes:")
	for _, l := range strings.Split(proj, "\n") {
		n := utf8.RuneCountInString(l)
		if strings.ContainsAny(l[:min(len(l), 3)], "┌│└") && n != boxWidth {
			t.Errorf("box line of %d runes: %q", n, l)
		}
		if n > 80 {
			t.Errorf("line of %d runes: %q", n, l)
		}
	}
}

// golden compares got with the file at path, or rewrites the file under
// -update.
func golden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- a fixed test file
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got == string(want) {
		return
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := range max(len(gl), len(wl)) {
		g, w := "", ""
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Errorf("%s:%d\n got: %q\nwant: %q", path, i+1, g, w)
		}
	}
}
