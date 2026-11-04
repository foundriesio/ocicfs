// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/composefs"
)

// mkfsCases are layer tars exercising each mkfs.erofs layout rule.
func mkfsCases() map[string][]tent {
	long := strings.Repeat("n", 200)
	var manyFiles, bigDir, deepTree []tent
	for i := range 300 {
		manyFiles = append(manyFiles, file(fmt.Sprintf("d%d/f%d", i%7, i), strings.Repeat("x", i)))
		bigDir = append(bigDir, file(fmt.Sprintf("big/%s-%03d", long[:i%120], i), ""))
	}
	for i := range 40 {
		deepTree = append(deepTree, dir(fmt.Sprintf("t%d/", i)), symlink(fmt.Sprintf("t%d/l", i), strings.Repeat("s", 30*i+1)))
	}
	return map[string][]tent{
		"root-only":   {dir("./")},
		"no-root":     {file("a", "hi")},
		"files":       {dir("./"), dir("etc/"), file("etc/a", strings.Repeat("a", 5000)), file("etc/b", "hello"), file("etc/empty", ""), file("etc/block", strings.Repeat("b", 8192))},
		"one-xattr":   {dir("./"), tent{name: "ping", body: "elf", xattrs: map[string]string{"security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"}}},
		"same-xattrs": {dir("./"), tent{name: "a", xattrs: map[string]string{"user.k": "v"}}, tent{name: "b", xattrs: map[string]string{"user.k": "v"}}, tent{name: "c", typ: tar.TypeDir, xattrs: map[string]string{"trusted.x": "yyyy", "user.k": "v"}}},
		"symlinks":    {dir("./"), symlink("short", "t"), symlink("long", strings.Repeat("p", 3000)), symlink("edge", strings.Repeat("q", 4096-64-1))},
		"hardlinks":   {dir("./"), file("a", strings.Repeat("h", 9000)), hardlink("b", "a"), hardlink("c", "a")},
		"node-links":  {dir("./"), symlink("s", "t"), hardlink("s2", "s"), symlink("long", strings.Repeat("p", 3000)), hardlink("long2", "long"), tent{name: "fifo", typ: tar.TypeFifo}, hardlink("fifo2", "fifo")},
		"replace":     {dir("./"), file("a", strings.Repeat("1", 9000)), file("a", "small"), dir("d/"), file("d/x", "x"), file("d", "now a file")},
		"implicit":    {file("a/b/c/d", "deep"), file("a/e", "e")},
		"whiteouts":   {dir("./"), dir("etc/"), whiteout("etc/gone"), whiteout("gone2"), dir("opq/"), file("opq/.wh..wh..opq", ""), file("opq/kept", "k")},
		"big-uid":     {dir("./"), tent{name: "f", uid: 70000, body: "u"}, tent{name: "d", typ: tar.TypeDir, uid: 1}},
		"many-files":  manyFiles,
		"big-dir":     bigDir,
		"tails":       deepTree,
		"devices":     {dir("./"), tent{name: "null", typ: tar.TypeChar}, tent{name: "fifo", typ: tar.TypeFifo}},
	}
}

// mkfsErofs returns the path of a mkfs.erofs 1.9.4, or skips.
func mkfsErofs(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		t.Skip("mkfs.erofs not found")
	}
	out, _ := exec.Command(p, "--version").CombinedOutput() // #nosec G204 -- test oracle found on PATH
	if !strings.Contains(string(out), " 1.9.4") {
		t.Skipf("mkfs.erofs is not 1.9.4: %s", strings.SplitN(string(out), "\n", 2)[0])
	}
	return p
}

// runMkfs builds an image from a tar with mkfs.erofs and returns its size.
func runMkfs(t *testing.T, mkfs string, raw []byte, args ...string) int64 {
	t.Helper()
	dir := t.TempDir()
	in, out := filepath.Join(dir, "layer.tar"), filepath.Join(dir, "layer.erofs")
	if err := os.WriteFile(in, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	argv := append(append([]string{}, args...), "--quiet", "-b4096", "-U", "c0ffee00-0000-4000-8000-000000000000", out, in)
	if b, err := exec.Command(mkfs, argv...).CombinedOutput(); err != nil { // #nosec G204 -- test oracle
		t.Fatalf("mkfs.erofs: %v: %s", err, b)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestMkfsErofsImageSizeMatchesMkfs(t *testing.T) {
	mkfs := mkfsErofs(t)
	for name, entries := range mkfsCases() {
		t.Run(name, func(t *testing.T) {
			raw := tarOf(t, entries...)
			want := runMkfs(t, mkfs, raw, "--tar=f", "--aufs", "-Enoinline_data")
			got, err := mkfsErofsImageSize(layerInv(t, raw), false)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("estimated %d, mkfs.erofs wrote %d", got, want)
			}
		})
	}
}

// headerballCases add chunk-map and alignment boundaries to the mkfs cases.
func headerballCases() map[string][]tent {
	cases := mkfsCases()
	for _, size := range []int{0, 1, 4096, 4097} {
		cases[fmt.Sprintf("size-%d", size)] = []tent{dir("./"), file("f", strings.Repeat("z", size))}
	}
	// A 64-byte extended inode plus an xattr body of 12 + redirect (4+8+66
	// rounded to 80) + metacopy (4+8+36 = 48) + user.<n> of 4+n+v: vary the
	// last to end the inode and xattrs at 8n-4, 8n and 8n+4.
	for _, n := range []int{1, 5, 9} {
		cases[fmt.Sprintf("align-%d", n)] = []tent{dir("./"), {name: "f", body: "data", xattrs: map[string]string{"user." + strings.Repeat("k", n): ""}}}
	}
	return cases
}

func TestHeaderballImageSizeMatchesMkfs(t *testing.T) {
	mkfs := mkfsErofs(t)
	for name, entries := range headerballCases() {
		t.Run(name, func(t *testing.T) {
			inv := layerInv(t, tarOf(t, entries...))
			hb := headerball(inv, 0)
			var hdrs []tent
			var sizes []int64
			for _, e := range entries {
				sizes = append(sizes, int64(len(e.body)))
				if e.typ == 0 && len(e.body) > 0 {
					d := digest.FromString(e.body).Encoded()
					x := map[string]string{"trusted.overlay.redirect": "/" + d[:2] + "/" + d[2:], "trusted.overlay.metacopy": "\x00\x24\x00\x01" + d[:32]}
					for k, v := range e.xattrs {
						x[k] = v
					}
					e.body, e.xattrs = "", x
				}
				hdrs = append(hdrs, e)
			}
			want := runMkfs(t, mkfs, headerOnlyTarOf(t, hdrs, sizes), "--tar=headerball", "--aufs")
			got, err := mkfsErofsImageSize(hb, true)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("estimated %d, mkfs.erofs wrote %d", got, want)
			}
		})
	}
}

func TestMkfsRejectsOpaqueRoot(t *testing.T) {
	inv := layerInv(t, tarOf(t, dir("./"), file(".wh..wh..opq", "")))
	if _, err := mkfsErofsImageSize(inv, false); err == nil {
		t.Fatal("an opaque marker at the root was accepted")
	}
}

// composefsCases are merged trees exercising each composefs-c layout rule.
func composefsCases() map[string][]composefs.Entry {
	dirE := func(p string) composefs.Entry {
		return composefs.Entry{Path: p, Mode: os.ModeDir | 0o755, Mtime: 1700000000}
	}
	ext := func(p string, size int64, content string) composefs.Entry {
		d := digest.FromString(content)
		return composefs.Entry{Path: p, Mode: 0o644, Size: size, Digest: d, Verity: d, Mtime: 1700000000}
	}
	inline := func(p, body string) composefs.Entry {
		return composefs.Entry{Path: p, Mode: 0o644, Size: int64(len(body)), Content: []byte(body), Mtime: 1700000000}
	}
	many := []composefs.Entry{dirE("/")}
	for i := range 400 {
		many = append(many, ext(fmt.Sprintf("/f%04d-%s", i, strings.Repeat("n", i%90)), int64(100+i), fmt.Sprint(i%50)))
	}
	tails := []composefs.Entry{dirE("/")}
	for i := range 60 {
		tails = append(tails, inline(fmt.Sprintf("/i%02d", i), strings.Repeat("c", i+5)), composefs.Entry{Path: fmt.Sprintf("/s%02d", i), Mode: os.ModeSymlink | 0o777, Linkname: strings.Repeat("t", 60*i+1), Mtime: 1700000000})
	}
	// Directories with spare room in their child lists, each holding
	// external files with distinct digests.
	subdirs := []composefs.Entry{dirE("/")}
	for d := range 5 {
		subdirs = append(subdirs, dirE(fmt.Sprintf("/d%d", d)))
		for f := range 40 {
			p := fmt.Sprintf("/d%d/f%d", d, f)
			subdirs = append(subdirs, ext(p, 1000, p))
		}
	}
	return map[string][]composefs.Entry{
		"subdirs":    subdirs,
		"empty":      {dirE("/")},
		"no-root":    {inline("/a", "x")},
		"files":      {dirE("/"), dirE("/etc"), ext("/etc/big", 100000, "a"), inline("/etc/small", "hello"), inline("/etc/empty", ""), ext("/etc/dup", 100000, "a")},
		"xattrs":     {dirE("/"), {Path: "/cap", Mode: 0o755, Size: 3, Content: []byte("elf"), Xattrs: map[string]string{"security.capability": "\x01\x00\x00\x02"}, Mtime: 1700000000}, {Path: "/cap2", Mode: 0o755, Size: 3, Content: []byte("elf"), Xattrs: map[string]string{"security.capability": "\x01\x00\x00\x02"}, Mtime: 1700000000}},
		"hardlink":   {dirE("/"), {Path: "/a", Mode: 0o644, Size: 9000, Digest: digest.FromString("h"), Verity: digest.FromString("h"), Nlink: 2, Mtime: 1700000000}, {Path: "/b", Hardlink: "/a"}},
		"whiteout":   {dirE("/"), {Path: "/.wh.x", Mode: os.ModeDevice | os.ModeCharDevice, Mtime: 1700000000}, {Path: "/c", Mode: os.ModeDevice | os.ModeCharDevice, Mtime: 1700000000}},
		"mtimes":     {dirE("/"), {Path: "/old", Mode: 0o644, Size: 1, Content: []byte("o"), Mtime: 1}, {Path: "/new", Mode: 0o644, Size: 1, Content: []byte("n"), Mtime: 1700000000, MtimeNsec: 5}},
		"many":       many,
		"tails":      tails,
		"size-limit": {dirE("/"), ext("/max", 1<<32-1, "m"), ext("/over", 1<<32, "o")},
	}
}

func TestComposefsImageSizeMatchesMkcomposefs(t *testing.T) {
	if err := composefs.Preflight(); err != nil {
		t.Skip(err)
	}
	for name, entries := range composefsCases() {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "image.cfs")
			if err := composefs.WriteImage(context.Background(), out, entries); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(out)
			if err != nil {
				t.Fatal(err)
			}
			got, err := composefsImageSize(entries, composefs.InlineContentMax)
			if err != nil {
				t.Fatal(err)
			}
			if got != st.Size() {
				t.Errorf("estimated %d, mkcomposefs wrote %d", got, st.Size())
			}
		})
	}
}

// headerOnlyTarOf returns a tar stream of entries whose regular files keep
// their sizes but carry no data, as a headerball stream does.
func headerOnlyTarOf(t testing.TB, entries []tent, sizes []int64) []byte {
	t.Helper()
	var out bytes.Buffer
	for i, e := range entries {
		h := e.header()
		size := sizes[i]
		h.Size = 0
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if err := tw.Flush(); err != nil {
			t.Fatal(err)
		}
		b := buf.Bytes()
		blk := b[len(b)-512:]
		copy(blk[124:136], []byte(fmt.Sprintf("%011o", size)+"\x00"))
		copy(blk[148:156], []byte("        "))
		var sum int64
		for _, c := range blk {
			sum += int64(c)
		}
		copy(blk[148:156], []byte(fmt.Sprintf("%06o", sum)+"\x00 "))
		out.Write(b)
	}
	out.Write(make([]byte, 1024))
	return out.Bytes()
}

func TestComposefsInlineAboveDefaultMatchesMkcomposefs(t *testing.T) {
	if err := composefs.Preflight(); err != nil {
		t.Skip(err)
	}
	// ocicfs never inlines more than 64 bytes, so the dump is written here:
	// a root and one inline file per size.
	sizes := []int{65, 2047, 2048, 2049, 4096, 4097, 5000}
	dump := "/ 0 40755 2 0 0 0 1700000000.0 - - -\n"
	entries := []composefs.Entry{{Path: "/", Mode: os.ModeDir | 0o755, Mtime: 1700000000}}
	for _, n := range sizes {
		p := fmt.Sprintf("/f%d", n)
		dump += fmt.Sprintf("%s %d 100644 1 0 0 0 1700000000.0 - %s -\n", p, n, strings.Repeat("c", n))
		d := digest.FromString(p)
		entries = append(entries, composefs.Entry{Path: p, Mode: 0o644, Size: int64(n), Digest: d, Verity: d, Mtime: 1700000000})
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "dump"), filepath.Join(dir, "image.cfs")
	if err := os.WriteFile(in, []byte(dump), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("mkcomposefs", "--min-version=1", "--max-version=1", "--from-file", in, out).CombinedOutput(); err != nil { // #nosec G204 -- test oracle
		t.Fatalf("mkcomposefs: %v: %s", err, b)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := composefsImageSize(entries, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if got != st.Size() {
		t.Errorf("estimated %d, mkcomposefs wrote %d", got, st.Size())
	}

	// One byte over the cap is refused.
	over := "/ 0 40755 2 0 0 0 1700000000.0 - - -\n/f 5001 100644 1 0 0 0 1700000000.0 - " + strings.Repeat("c", 5001) + " -\n"
	if err := os.WriteFile(in, []byte(over), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("mkcomposefs", "--min-version=1", "--max-version=1", "--from-file", in, out).Run(); err == nil { // #nosec G204 -- test oracle
		t.Errorf("mkcomposefs inlined %d bytes", composefsInlineCap+1)
	}
}
