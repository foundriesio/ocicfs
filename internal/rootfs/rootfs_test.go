// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/objectstore"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "etc/passwd", "etc/passwd", false},
		{"clean", "etc/./passwd", "etc/passwd", false},
		{"absolute", "/etc/passwd", "etc/passwd", false},
		{"root dot", ".", "", false},
		{"root slash", "/", "", false},
		{"root dot-slash", "./", "", false},
		{"escape", "../../etc/passwd", "", true},
		{"escape after clean", "a/../../etc/passwd", "", true},
		{"absolute escape", "/../etc/passwd", "", true},
		{"clean root alias", "a/..", "", false},
		{"nul", "etc\x00passwd", "", true},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeName(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeName: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRemoveSubtree(t *testing.T) {
	tree := map[string]*node{
		"dir":       {mode: os.ModeDir | 0o755},
		"dir/file":  {mode: 0o644},
		"dir/sub/f": {mode: 0o644},
		"other":     {mode: 0o644},
	}
	removeSubtree(tree, "dir")
	if _, ok := tree["dir"]; ok {
		t.Fatal("dir survived")
	}
	if _, ok := tree["dir/file"]; ok {
		t.Fatal("dir/file survived")
	}
	if _, ok := tree["other"]; !ok {
		t.Fatal("other was removed")
	}
}

func TestRemoveOpaqueRemovesOnlyLowerDescendants(t *testing.T) {
	tree := map[string]*node{
		"dir":         {mode: os.ModeDir | 0o755, layer: 0},
		"dir/old":     {mode: 0o644, layer: 0},
		"dir/current": {mode: 0o644, layer: 2},
		"other/old":   {mode: 0o644, layer: 0},
	}
	removeOpaque(tree, "dir", 2)
	if _, ok := tree["dir/old"]; ok {
		t.Fatal("lower descendant survived")
	}
	if _, ok := tree["dir/current"]; !ok {
		t.Fatal("same-layer descendant was removed")
	}
	if _, ok := tree["dir"]; !ok {
		t.Fatal("opaque removed the directory itself")
	}
	if _, ok := tree["other/old"]; !ok {
		t.Fatal("sibling was removed")
	}
}

func TestEnsureParents(t *testing.T) {
	tree := map[string]*node{"a/b/c": {mode: 0o644}}
	if err := ensureParents(tree); err != nil {
		t.Fatalf("ensureParents: %v", err)
	}
	for _, p := range []string{"a", "a/b"} {
		if n, ok := tree[p]; !ok || n.mode&os.ModeDir == 0 {
			t.Fatalf("parent %s missing or not a directory", p)
		}
	}

	tree = map[string]*node{
		"a":   {mode: 0o644},
		"a/b": {mode: 0o644},
	}
	if err := ensureParents(tree); err == nil {
		t.Fatal("expected non-directory parent error")
	}
}

func TestCloneHardlink(t *testing.T) {
	content := digest.SHA256.FromString("content")
	verity := digest.SHA256.FromString("verity")
	tree := map[string]*node{
		"target": {
			mode:   0o600,
			uid:    10,
			gid:    20,
			xattrs: map[string]string{"user.k": "v"},
			digest: content,
			verity: verity,
			size:   5,
			layer:  1,
		},
	}
	if err := cloneHardlink(tree, "link", "target", 2); err != nil {
		t.Fatalf("cloneHardlink: %v", err)
	}
	got := tree["link"]
	if got.digest != content || got.verity != verity || got.layer != 2 || got.uid != 10 || got.gid != 20 {
		t.Fatalf("clone = %#v", got)
	}
	got.xattrs["user.k"] = "changed"
	if tree["target"].xattrs["user.k"] != "v" {
		t.Fatal("xattrs were not cloned")
	}
}

func TestCloneHardlinkRejectsNonRegular(t *testing.T) {
	tree := map[string]*node{"target": {mode: os.ModeSymlink | 0o777}}
	if err := cloneHardlink(tree, "link", "target", 1); err == nil {
		t.Fatal("expected non-regular target error")
	}
	if err := cloneHardlink(tree, "missing-link", "missing", 1); err == nil {
		t.Fatal("expected missing target error")
	}
}

func TestBuildEntriesRejectsEscapingName(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "../../x", typ: tar.TypeDir, mode: 0o755})
	_, err := buildEntries(context.Background(), nil, []io.Reader{bytes.NewReader(layer)}, []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected escaping path error")
	}
}

func TestBuildEntriesRejectsNonRegularHardlinkTarget(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "target", typ: tar.TypeSymlink, mode: 0o777, link: "somewhere"},
		tarEntry{name: "link", typ: tar.TypeLink, link: "target"},
	)
	_, err := buildEntries(context.Background(), nil, []io.Reader{bytes.NewReader(layer)}, []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected non-regular hardlink target error")
	}
}

func TestBuildEntriesRejectsForwardHardlink(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "link", typ: tar.TypeLink, link: "target"},
		tarEntry{name: "target", typ: tar.TypeSymlink, mode: 0o777, link: "somewhere"},
	)
	_, err := buildEntries(context.Background(), nil, []io.Reader{bytes.NewReader(layer)}, []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected missing hardlink target error")
	}
}

func TestBuildEntriesChecksDiffID(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "dir", typ: tar.TypeDir, mode: 0o755})
	wrong := digest.SHA256.FromString("wrong")
	_, err := buildEntries(context.Background(), nil, []io.Reader{bytes.NewReader(layer)}, []digest.Digest{wrong})
	if err == nil {
		t.Fatal("expected diff_id mismatch")
	}
	if !strings.Contains(err.Error(), "diff_id mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildEntriesWithStore(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o644, body: "v1"},
		tarEntry{name: "usr/bin/app", typ: tar.TypeReg, mode: 0o755, body: "app"},
		tarEntry{name: "var/lib", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "var/lib/old", typ: tar.TypeReg, mode: 0o644, body: "old"},
		tarEntry{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "etc/config"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "etc/.wh.config", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o600, body: "v2", pax: map[string]string{"SCHILY.xattr.user.test": "yes"}},
		tarEntry{name: "app-copy", typ: tar.TypeLink, link: "usr/bin/app"},
		tarEntry{name: "var/lib/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "var/lib/new", typ: tar.TypeReg, mode: 0o644, body: "new"},
	)
	entries, err := buildEntries(context.Background(), store, []io.Reader{bytes.NewReader(layer1), bytes.NewReader(layer2)}, []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/var/lib/old"]; ok {
		t.Fatal("opaque marker did not remove lower file")
	}
	cfg := byPath["/etc/config"]
	if cfg.Digest != digest.FromBytes([]byte("v2")) {
		t.Fatalf("/etc/config digest = %s", cfg.Digest)
	}
	if cfg.Verity == "" {
		t.Fatal("/etc/config missing fs-verity digest")
	}
	if cfg.Mode.Perm() != 0o600 || cfg.Xattrs["user.test"] != "yes" {
		t.Fatalf("/etc/config metadata = %#v", cfg)
	}
	app := byPath["/usr/bin/app"]
	link := byPath["/app-copy"]
	if link.Digest != app.Digest || link.Verity != app.Verity {
		t.Fatalf("hardlink did not clone backing digests: app=%#v link=%#v", app, link)
	}
	if byPath["/link"].Linkname != "etc/config" {
		t.Fatalf("symlink = %#v", byPath["/link"])
	}
}

// TestBuildEntriesModifyWithoutWhiteout asserts that a regular file re-emitted in
// an upper layer overrides the lower layer's version -- content and metadata --
// with no whiteout marker, the last-writer-wins path a plain file edit takes.
func TestBuildEntriesModifyWithoutWhiteout(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o644, body: "v1"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o600, body: "v2", pax: map[string]string{"SCHILY.xattr.user.test": "yes"}},
	)
	entries, err := buildEntries(context.Background(), store, []io.Reader{bytes.NewReader(layer1), bytes.NewReader(layer2)}, []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	cfg := entriesByPath(entries)["/etc/config"]
	if cfg.Digest != digest.FromBytes([]byte("v2")) {
		t.Fatalf("/etc/config digest = %s, want v2 content", cfg.Digest)
	}
	if cfg.Mode.Perm() != 0o600 || cfg.Xattrs["user.test"] != "yes" {
		t.Fatalf("/etc/config not overridden by upper layer: %#v", cfg)
	}
}

// TestBuildEntriesReplaceDirWithFile asserts that when an upper layer replaces a
// directory with a non-directory and no whiteout precedes it, the lower layer's
// children are dropped rather than left orphaned under a non-directory parent
// (which would otherwise fail the parent-before-child merge).
func TestBuildEntriesReplaceDirWithFile(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "lib", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "lib/old", typ: tar.TypeReg, mode: 0o644, body: "old"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "lib", typ: tar.TypeSymlink, mode: 0o777, link: "usr/lib"},
	)
	entries, err := buildEntries(context.Background(), store, []io.Reader{bytes.NewReader(layer1), bytes.NewReader(layer2)}, []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/lib/old"]; ok {
		t.Fatal("child of replaced directory was not dropped")
	}
	if lib := byPath["/lib"]; lib.Linkname != "usr/lib" {
		t.Fatalf("/lib not replaced by symlink: %#v", lib)
	}
}

func TestBuild(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	store := newVerityStore(t)
	layer := tarBytes(t, tarEntry{name: "hello", typ: tar.TypeReg, mode: 0o644, body: "world"})
	img, err := Assemble(context.Background(), store, []io.Reader{bytes.NewReader(layer)}, []digest.Digest{digest.FromBytes(layer)})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if img.Descriptor.MediaType != MediaTypeComposefsImage {
		t.Fatalf("mediaType = %q", img.Descriptor.MediaType)
	}
	if err := img.Descriptor.Digest.Validate(); err != nil {
		t.Fatalf("descriptor digest invalid: %v", err)
	}
	if img.Descriptor.Size <= 0 {
		t.Fatalf("descriptor size = %d", img.Descriptor.Size)
	}
	if _, err := digestutil.ParseSHA256(img.FsverityDigest); err != nil {
		t.Fatalf("fsverity digest invalid: %v", err)
	}
}

func newVerityStore(t *testing.T) *objectstore.Store {
	t.Helper()
	root, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	s, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	return s
}

func skipIfNoVerity(t *testing.T, err error) {
	t.Helper()
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
}

func entriesByPath(entries []composefs.Entry) map[string]composefs.Entry {
	out := make(map[string]composefs.Entry, len(entries))
	for _, e := range entries {
		out[e.Path] = e
	}
	return out
}

type tarEntry struct {
	name string
	typ  byte
	mode int64
	body string
	link string
	pax  map[string]string
}

func tarBytes(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:       e.name,
			Typeflag:   e.typ,
			Mode:       e.mode,
			Linkname:   e.link,
			Size:       int64(len(e.body)),
			PAXRecords: e.pax,
		}
		if e.typ != tar.TypeReg && e.typ != '\x00' {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %s: %v", e.name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}
	return buf.Bytes()
}
