// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/digestutil"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/verity"
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
	synthesized, err := ensureParents(tree)
	if err != nil {
		t.Fatalf("ensureParents: %v", err)
	}
	if !synthesized {
		t.Fatal("expected synthesized parents to be reported")
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
	if _, err := ensureParents(tree); err == nil {
		t.Fatal("expected non-directory parent error")
	}
}

func TestLinkHardlink(t *testing.T) {
	target := &node{mode: 0o600, digest: digest.SHA256.FromString("content"), layer: 1}
	tree := map[string]*node{"target": target}
	if err := linkHardlink(tree, "link", "target", 2); err != nil {
		t.Fatalf("linkHardlink: %v", err)
	}
	if got := tree["link"]; got.link != target || got.layer != 2 {
		t.Fatalf("link node = %#v, want target's payload node at layer 2", got)
	}
	// A link to a link points at the payload node directly, never forming a chain.
	if err := linkHardlink(tree, "link2", "link", 3); err != nil {
		t.Fatalf("linkHardlink to a link: %v", err)
	}
	if got := tree["link2"]; got.link != target {
		t.Fatalf("link-to-link node = %#v, want target's payload node", got)
	}
}

func TestLinkHardlinkTargetChecks(t *testing.T) {
	tree := map[string]*node{
		"dir":     {mode: os.ModeDir | 0o755},
		"symlink": {mode: os.ModeSymlink | 0o777, linkname: "somewhere"},
	}
	if err := linkHardlink(tree, "link", "dir", 1); err == nil {
		t.Fatal("expected directory target error")
	}
	if err := linkHardlink(tree, "link", "missing", 1); err == nil {
		t.Fatal("expected missing target error")
	}
	if err := linkHardlink(tree, "link", "symlink", 1); err != nil {
		t.Fatalf("hardlink to a symlink must be allowed: %v", err)
	}
}

func TestBuildEntriesRejectsEscapingName(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "../../x", typ: tar.TypeDir, mode: 0o755})
	_, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected escaping path error")
	}
}

func TestBuildEntriesHardlinkToSymlink(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "target", typ: tar.TypeSymlink, mode: 0o777, link: "somewhere"},
		tarEntry{name: "link", typ: tar.TypeLink, link: "target"},
	)
	entries, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	// "/link" sorts first, so it carries the shared inode's payload and the
	// original "/target" name becomes the hardlink entry referencing it.
	if got := byPath["/link"]; got.Linkname != "somewhere" || got.Hardlink != "" {
		t.Fatalf("/link = %#v, want the symlink payload", got)
	}
	if got := byPath["/target"]; got.Hardlink != "/link" {
		t.Fatalf("/target = %#v, want a hardlink entry to /link", got)
	}
}

func TestBuildEntriesRejectsForwardHardlink(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "link", typ: tar.TypeLink, link: "target"},
		tarEntry{name: "target", typ: tar.TypeSymlink, mode: 0o777, link: "somewhere"},
	)
	_, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected missing hardlink target error")
	}
}

func TestBuildEntriesChecksDiffID(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "dir", typ: tar.TypeDir, mode: 0o755})
	wrong := digest.SHA256.FromString("wrong")
	_, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{wrong})
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
		tarEntry{name: "usr/bin/app", typ: tar.TypeReg, mode: 0o755, body: large("app")},
		tarEntry{name: "var/lib", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "var/lib/old", typ: tar.TypeReg, mode: 0o644, body: "old"},
		tarEntry{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "etc/config"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "etc/.wh.config", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o600, body: large("v2"), pax: map[string]string{"SCHILY.xattr.security.capability": "cap"}},
		tarEntry{name: "app-copy", typ: tar.TypeLink, link: "usr/bin/app"},
		tarEntry{name: "var/lib/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "var/lib/new", typ: tar.TypeReg, mode: 0o644, body: "new"},
	)
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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
	if cfg.Digest != digest.FromBytes([]byte(large("v2"))) {
		t.Fatalf("/etc/config digest = %s", cfg.Digest)
	}
	if cfg.Verity == "" {
		t.Fatal("/etc/config missing fs-verity digest")
	}
	if cfg.Mode.Perm() != 0o600 || cfg.Xattrs["security.capability"] != "cap" {
		t.Fatalf("/etc/config metadata = %#v", cfg)
	}
	// "/app-copy" sorts before "/usr/bin/app", so it carries the shared inode's
	// payload and the original name is emitted as the hardlink entry.
	appCopy := byPath["/app-copy"]
	if appCopy.Digest != digest.FromBytes([]byte(large("app"))) || appCopy.Verity == "" || appCopy.Hardlink != "" || appCopy.Nlink != 2 {
		t.Fatalf("/app-copy = %#v, want the shared inode payload with Nlink 2", appCopy)
	}
	if got := byPath["/usr/bin/app"]; got.Hardlink != "/app-copy" {
		t.Fatalf("/usr/bin/app = %#v, want a hardlink entry to /app-copy", got)
	}
	if byPath["/link"].Linkname != "etc/config" {
		t.Fatalf("symlink = %#v", byPath["/link"])
	}
}

// TestContentSizeMergedTree asserts the rootfs size reflects the merged tree:
// an overwritten file counts its final content only, a whited-out file does
// not count, a hardlink group counts once, and metadata-only entries
// (directories, symlinks) contribute nothing.
func TestContentSizeMergedTree(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o644, body: "original content"}, // replaced in layer2
		tarEntry{name: "usr/bin/app", typ: tar.TypeReg, mode: 0o755, body: "application"},     // hardlinked in layer2
		tarEntry{name: "var/junk", typ: tar.TypeReg, mode: 0o644, body: "to be removed"},      // whited out in layer2
		tarEntry{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "etc/config"},
		tarEntry{name: "var/lib", typ: tar.TypeDir, mode: 0o755},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o644, body: "v2"},
		tarEntry{name: "app-copy", typ: tar.TypeLink, link: "usr/bin/app"},
		tarEntry{name: "var/.wh.junk", typ: tar.TypeReg, mode: 0o000},
	)
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	want := int64(len("v2") + len("application"))
	if got := contentSize(entries); got != want {
		t.Fatalf("contentSize = %d, want %d (final config + app counted once, junk removed)", got, want)
	}
}

// TestBuildEntriesHardlinkSurvivesTargetWhiteout asserts that whiting out one
// name of a multiply-linked inode removes only that name: the remaining names
// keep the content, one of them becoming the payload entry.
func TestBuildEntriesHardlinkSurvivesTargetWhiteout(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: large("app")},
		tarEntry{name: "bin/alias1", typ: tar.TypeLink, link: "bin/app"},
		tarEntry{name: "bin/alias2", typ: tar.TypeLink, link: "bin/alias1"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "bin/.wh.app", typ: tar.TypeReg, mode: 0o000},
	)
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/bin/app"]; ok {
		t.Fatal("whiteout did not remove the target name")
	}
	if got := byPath["/bin/alias1"]; got.Digest != digest.FromBytes([]byte(large("app"))) || got.Mode.Perm() != 0o755 || got.Hardlink != "" || got.Nlink != 2 {
		t.Fatalf("/bin/alias1 = %#v, want the surviving payload entry with Nlink 2", got)
	}
	if got := byPath["/bin/alias2"]; got.Hardlink != "/bin/alias1" {
		t.Fatalf("/bin/alias2 = %#v, want a hardlink entry to /bin/alias1", got)
	}
}

// TestBuildEntriesHardlinkTargetReplaced asserts that re-emitting one name of a
// multiply-linked inode in an upper layer detaches only that name: it gets the
// new content while the other names keep the old inode, matching what an
// unlink-and-create sequence does to a hardlink group on a real filesystem.
func TestBuildEntriesHardlinkTargetReplaced(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: large("v1")},
		tarEntry{name: "bin/alias", typ: tar.TypeLink, link: "bin/app"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: large("v2")},
	)
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	if got := byPath["/bin/app"]; got.Digest != digest.FromBytes([]byte(large("v2"))) || got.Hardlink != "" {
		t.Fatalf("/bin/app = %#v, want the replacing v2 payload", got)
	}
	if got := byPath["/bin/alias"]; got.Digest != digest.FromBytes([]byte(large("v1"))) || got.Hardlink != "" {
		t.Fatalf("/bin/alias = %#v, want the original v1 payload", got)
	}
}

// TestBuildEntriesModifyWithoutWhiteout asserts that a regular file re-emitted in
// an upper layer overrides the lower layer's version -- content and metadata --
// with no whiteout marker, the last-writer-wins path a plain file edit takes.
func TestBuildEntriesModifyWithoutWhiteout(t *testing.T) {
	store := newVerityStore(t)
	layer1 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o644, body: large("v1")},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o600, body: large("v2"), pax: map[string]string{"SCHILY.xattr.security.capability": "cap"}},
	)
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	cfg := entriesByPath(entries)["/etc/config"]
	if cfg.Digest != digest.FromBytes([]byte(large("v2"))) {
		t.Fatalf("/etc/config digest = %s, want v2 content", cfg.Digest)
	}
	if cfg.Mode.Perm() != 0o600 || cfg.Xattrs["security.capability"] != "cap" {
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
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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

// TestBuildEntriesInlinesSmallFiles asserts the inline boundary: files of 0, 1,
// and exactly InlineContentMax bytes carry their bytes in Content and are not
// written to the object store, while a file one byte larger is stored by digest.
func TestBuildEntriesInlinesSmallFiles(t *testing.T) {
	store := newVerityStore(t)
	files := []struct {
		name   string
		body   string
		inline bool
	}{
		{"empty", "", true},
		{"one", "a", true},
		{"max", strings.Repeat("b", composefs.InlineContentMax), true},
		{"over", strings.Repeat("c", composefs.InlineContentMax+1), false},
	}
	te := make([]tarEntry, 0, len(files))
	for _, f := range files {
		te = append(te, tarEntry{name: f.name, typ: tar.TypeReg, mode: 0o644, body: f.body})
	}
	layer := tarBytes(t, te...)
	entries, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	for _, f := range files {
		e := byPath["/"+f.name]
		content := digest.FromBytes([]byte(f.body))
		if f.inline {
			if e.Content == nil || len(e.Content) != len(f.body) {
				t.Fatalf("/%s = %#v, want %d inline bytes", f.name, e, len(f.body))
			}
			if e.Digest != "" || e.Verity != "" {
				t.Fatalf("/%s inlined but carries a digest: %#v", f.name, e)
			}
			assertObjectAbsent(t, store, content)
		} else {
			if e.Content != nil {
				t.Fatalf("/%s = %#v, want by-digest storage, not inline", f.name, e)
			}
			if e.Digest != content || e.Verity == "" {
				t.Fatalf("/%s = %#v, want a stored object with a digest and fs-verity", f.name, e)
			}
			assertObjectPresent(t, store, content)
		}
	}
}

// TestBuildEntriesHardlinkToInlinedFile asserts a hardlink whose target is a
// small inlined file round-trips: the target entry carries the inline content
// and the shared name count, and the link entry references it.
func TestBuildEntriesHardlinkToInlinedFile(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "cfg", typ: tar.TypeReg, mode: 0o644, body: "k=v\n"},
		tarEntry{name: "cfg2", typ: tar.TypeLink, link: "cfg"},
	)
	entries, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	// "/cfg" sorts before "/cfg2", so it carries the inline payload and the count.
	if got := byPath["/cfg"]; string(got.Content) != "k=v\n" || got.Digest != "" || got.Hardlink != "" || got.Nlink != 2 {
		t.Fatalf("/cfg = %#v, want the inlined payload with Nlink 2", got)
	}
	if got := byPath["/cfg2"]; got.Hardlink != "/cfg" {
		t.Fatalf("/cfg2 = %#v, want a hardlink entry to /cfg", got)
	}
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		return
	}
	out := filepath.Join(t.TempDir(), "image.cfs")
	if err := composefs.WriteImage(context.Background(), out, entries); err != nil {
		t.Fatalf("WriteImage on a hardlink-to-inline image: %v", err)
	}
}

// TestBuildEntriesPreservesMtimes asserts each node kind carries its tar
// modification time (including sub-second precision from a PAX record) while a
// synthesized parent directory, which has no tar header, keeps mtime 0. Device
// nodes read their mtime through the same header path as the FIFO covered here.
func TestBuildEntriesPreservesMtimes(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "etc/config", typ: tar.TypeReg, mode: 0o644, body: "k=v\n", mtime: time.Unix(1700000000, 123456789)},
		tarEntry{name: "dir", typ: tar.TypeDir, mode: 0o755, mtime: time.Unix(1600000000, 0)},
		tarEntry{name: "sym", typ: tar.TypeSymlink, mode: 0o777, link: "etc/config", mtime: time.Unix(1500000000, 0)},
		tarEntry{name: "fifo", typ: tar.TypeFifo, mode: 0o644, mtime: time.Unix(1400000000, 0)},
	)
	entries, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	if e := byPath["/etc/config"]; e.Mtime != 1700000000 || e.MtimeNsec != 123456789 {
		t.Fatalf("/etc/config mtime = %d.%09d, want 1700000000.123456789", e.Mtime, e.MtimeNsec)
	}
	if e := byPath["/dir"]; e.Mtime != 1600000000 || e.MtimeNsec != 0 {
		t.Fatalf("/dir mtime = %d.%09d, want 1600000000.0", e.Mtime, e.MtimeNsec)
	}
	if e := byPath["/sym"]; e.Mtime != 1500000000 {
		t.Fatalf("/sym mtime = %d, want 1500000000", e.Mtime)
	}
	if e := byPath["/fifo"]; e.Mtime != 1400000000 {
		t.Fatalf("/fifo mtime = %d, want 1400000000", e.Mtime)
	}
	if e := byPath["/etc"]; e.Mtime != 0 || e.MtimeNsec != 0 {
		t.Fatalf("synthesized /etc mtime = %d.%09d, want 0", e.Mtime, e.MtimeNsec)
	}
	var dump bytes.Buffer
	if err := composefs.WriteDump(&dump, entries); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	if !strings.Contains(dump.String(), " 1700000000.123456789 ") {
		t.Fatalf("dump missing nanosecond mtime column:\n%s", dump.String())
	}
}

// TestBuildEntriesClampsPreEpochMtime asserts a pre-epoch tar mtime clamps to 0,
// since the composefs-dump parser reads the MTIME column as unsigned.
func TestBuildEntriesClampsPreEpochMtime(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "f", typ: tar.TypeReg, mode: 0o644, body: "x", mtime: time.Unix(-5, 0)})
	entries, _, err := BuildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	if e := entriesByPath(entries)["/f"]; e.Mtime != 0 || e.MtimeNsec != 0 {
		t.Fatalf("pre-epoch mtime not clamped: %d.%09d", e.Mtime, e.MtimeNsec)
	}
}

// TestTransformCanonicalStandardImage asserts a standard-layout image (every
// parent present, /usr a directory, no odd /run) is reported canonical and that
// the root directory adopts /usr's metadata: mode, uid, gid, mtime, and the
// allowlisted xattr, with the non-allowlisted one dropped.
func TestTransformCanonicalStandardImage(t *testing.T) {
	usrMtime := time.Unix(1710000000, 42)
	layer := tarBytes(t,
		tarEntry{
			name: "usr", typ: tar.TypeDir, mode: 0o750, uid: 5, gid: 6, mtime: usrMtime,
			pax: map[string]string{"SCHILY.xattr.security.capability": "cap", "SCHILY.xattr.user.foo": "bar"},
		},
		tarEntry{name: "usr/lib", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "usr/lib/f", typ: tar.TypeReg, mode: 0o644, body: "hi"},
	)
	entries, canonical := buildInline(t, layer)
	if !canonical {
		t.Fatal("standard image reported non-canonical")
	}
	root := entriesByPath(entries)["/"]
	if root.Mode != os.ModeDir|0o750 || root.UID != 5 || root.GID != 6 {
		t.Fatalf("root metadata = %#v, want /usr's mode/uid/gid", root)
	}
	if root.Mtime != usrMtime.Unix() || root.MtimeNsec != int64(usrMtime.Nanosecond()) {
		t.Fatalf("root mtime = %d.%09d, want /usr's %d.%09d", root.Mtime, root.MtimeNsec, usrMtime.Unix(), usrMtime.Nanosecond())
	}
	if root.Xattrs["security.capability"] != "cap" || len(root.Xattrs) != 1 {
		t.Fatalf("root xattrs = %#v, want only security.capability", root.Xattrs)
	}
}

// TestTransformMissingUsrUsesDefaultRoot asserts an image without /usr is
// non-canonical and its root falls back to the canonical default metadata.
func TestTransformMissingUsrUsesDefaultRoot(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "etc", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "etc/f", typ: tar.TypeReg, mode: 0o644, body: "hi"},
	)
	entries, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("missing /usr reported canonical")
	}
	root := entriesByPath(entries)["/"]
	if root.Mode != os.ModeDir|0o755 || root.UID != 0 || root.GID != 0 || root.Mtime != 0 || root.MtimeNsec != 0 || root.Xattrs != nil {
		t.Fatalf("root metadata = %#v, want canonical default", root)
	}
}

// TestTransformUsrNotDirectoryDiverges asserts a /usr that is not a directory (a
// symlink here) is treated like a missing /usr: default root, non-canonical.
func TestTransformUsrNotDirectoryDiverges(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeSymlink, mode: 0o777, link: "share"},
		tarEntry{name: "share", typ: tar.TypeDir, mode: 0o755},
	)
	entries, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("symlink /usr reported canonical")
	}
	byPath := entriesByPath(entries)
	if byPath["/usr"].Linkname != "share" {
		t.Fatalf("/usr = %#v, want the symlink left in place", byPath["/usr"])
	}
	if root := byPath["/"]; root.Mode != os.ModeDir|0o755 {
		t.Fatalf("root mode = %v, want canonical default", root.Mode)
	}
}

// TestTransformEmptiesRun asserts /run is emptied in place (node kept, children
// removed) with its mtime taken from /usr, and the image stays canonical.
func TestTransformEmptiesRun(t *testing.T) {
	usrMtime := time.Unix(1710000000, 7)
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755, mtime: usrMtime},
		tarEntry{name: "run", typ: tar.TypeDir, mode: 0o755, mtime: time.Unix(1600000000, 0)},
		tarEntry{name: "run/foo", typ: tar.TypeReg, mode: 0o644, body: "x"},
		tarEntry{name: "run/sub", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "run/sub/bar", typ: tar.TypeReg, mode: 0o644, body: "y"},
	)
	entries, canonical := buildInline(t, layer)
	if !canonical {
		t.Fatal("populated /run (directory) reported non-canonical")
	}
	byPath := entriesByPath(entries)
	run, ok := byPath["/run"]
	if !ok || run.Mode&os.ModeDir == 0 {
		t.Fatalf("/run node not kept: %#v", run)
	}
	if run.Mtime != usrMtime.Unix() || run.MtimeNsec != int64(usrMtime.Nanosecond()) {
		t.Fatalf("/run mtime = %d.%09d, want /usr's %d.%09d", run.Mtime, run.MtimeNsec, usrMtime.Unix(), usrMtime.Nanosecond())
	}
	for _, p := range []string{"/run/foo", "/run/sub", "/run/sub/bar"} {
		if _, ok := byPath[p]; ok {
			t.Fatalf("%s survived /run canonicalization", p)
		}
	}
}

// TestTransformMissingUsrPopulatedRun exercises the fallback metadata source
// end to end: /run is emptied using the default mtime 0, and the combined image
// is non-canonical.
func TestTransformMissingUsrPopulatedRun(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "run", typ: tar.TypeDir, mode: 0o755, mtime: time.Unix(1600000000, 0)},
		tarEntry{name: "run/foo", typ: tar.TypeReg, mode: 0o644, body: "x"},
	)
	entries, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("missing /usr with populated /run reported canonical")
	}
	byPath := entriesByPath(entries)
	if run := byPath["/run"]; run.Mode&os.ModeDir == 0 || run.Mtime != 0 || run.MtimeNsec != 0 {
		t.Fatalf("/run = %#v, want kept with default mtime 0", run)
	}
	if _, ok := byPath["/run/foo"]; ok {
		t.Fatal("/run/foo survived canonicalization")
	}
}

// TestTransformNonDirectoryRunDiverges asserts a /run that is not a directory is
// left untouched and marks the image non-canonical.
func TestTransformNonDirectoryRunDiverges(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "run", typ: tar.TypeReg, mode: 0o644, body: "not a dir"},
	)
	entries, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("non-directory /run reported canonical")
	}
	if run := entriesByPath(entries)["/run"]; run.Mode&os.ModeDir != 0 || string(run.Content) != "not a dir" {
		t.Fatalf("/run = %#v, want the file left in place", run)
	}
}

// TestTransformSynthesizedParentDiverges asserts that an image relying on an
// implicit parent directory (which the builder synthesizes) is non-canonical.
func TestTransformSynthesizedParentDiverges(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a/b/c", typ: tar.TypeReg, mode: 0o644, body: "deep"},
	)
	entries, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("implicit-parent image reported canonical")
	}
	byPath := entriesByPath(entries)
	for _, p := range []string{"/a", "/a/b"} {
		if n, ok := byPath[p]; !ok || n.Mode&os.ModeDir == 0 {
			t.Fatalf("synthesized parent %s missing or not a directory: %#v", p, n)
		}
	}
}

// TestTransformOpaqueSameLayerBeforeMarkerDiverges asserts the divergence case:
// a same-layer entry written under a directory *before* that layer's opaque
// marker is kept by ocicfs (upstream clears it), so the image is non-canonical.
func TestTransformOpaqueSameLayerBeforeMarkerDiverges(t *testing.T) {
	layer1 := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "d", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "d/lower", typ: tar.TypeReg, mode: 0o644, body: "lower"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "d/before", typ: tar.TypeReg, mode: 0o644, body: "before"},
		tarEntry{name: "d/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
	)
	entries, canonical := buildInline(t, layer1, layer2)
	if canonical {
		t.Fatal("opaque-with-same-layer-before reported canonical")
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/d/before"]; !ok {
		t.Fatal("/d/before (same layer as the marker) was dropped")
	}
	if _, ok := byPath["/d/lower"]; ok {
		t.Fatal("/d/lower (lower layer) survived the opaque marker")
	}
}

// TestTransformOpaqueSameLayerAfterMarkerStaysCanonical is the control: when the
// opaque marker precedes its layer's own entries — the usual tar ordering —
// ocicfs and upstream agree, so the image stays canonical.
func TestTransformOpaqueSameLayerAfterMarkerStaysCanonical(t *testing.T) {
	layer1 := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "d", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "d/lower", typ: tar.TypeReg, mode: 0o644, body: "lower"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "d/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "d/after", typ: tar.TypeReg, mode: 0o644, body: "after"},
	)
	entries, canonical := buildInline(t, layer1, layer2)
	if !canonical {
		t.Fatal("opaque-then-same-layer-entries reported non-canonical")
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/d/after"]; !ok {
		t.Fatal("/d/after was dropped")
	}
	if _, ok := byPath["/d/lower"]; ok {
		t.Fatal("/d/lower survived the opaque marker")
	}
}

// TestTransformXattrAllowlist asserts a non-root node keeps only
// security.capability and drops every other xattr.
func TestTransformXattrAllowlist(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{
			name: "f", typ: tar.TypeReg, mode: 0o644, body: "hi",
			pax: map[string]string{
				"SCHILY.xattr.security.capability": "cap",
				"SCHILY.xattr.security.selinux":    "label",
				"SCHILY.xattr.user.foo":            "bar",
			},
		},
	)
	entries, _ := buildInline(t, layer)
	f := entriesByPath(entries)["/f"]
	if len(f.Xattrs) != 1 || f.Xattrs["security.capability"] != "cap" {
		t.Fatalf("/f xattrs = %#v, want only security.capability", f.Xattrs)
	}
}

// TestTransformFiltersWhitedOutHardlinkPayloadXattr pins that a hardlink whose
// payload's own name was whited out still has its xattrs filtered: only the
// wrapper remains in the tree, but its payload — reached through the link — must
// drop disallowed xattrs. The image is otherwise canonical.
func TestTransformFiltersWhitedOutHardlinkPayloadXattr(t *testing.T) {
	layer1 := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "bin", typ: tar.TypeDir, mode: 0o755},
		tarEntry{
			name: "bin/f", typ: tar.TypeReg, mode: 0o644, body: "data",
			pax: map[string]string{"SCHILY.xattr.security.capability": "cap", "SCHILY.xattr.user.foo": "bar"},
		},
		tarEntry{name: "bin/g", typ: tar.TypeLink, link: "bin/f"},
	)
	layer2 := tarBytes(t, tarEntry{name: "bin/.wh.f", typ: tar.TypeReg, mode: 0o000})
	entries, canonical := buildInline(t, layer1, layer2)
	if !canonical {
		t.Fatal("hardlink+whiteout image reported non-canonical")
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/bin/f"]; ok {
		t.Fatal("/bin/f survived its whiteout")
	}
	g := byPath["/bin/g"]
	if string(g.Content) != "data" {
		t.Fatalf("/bin/g = %#v, want the surviving payload", g)
	}
	if len(g.Xattrs) != 1 || g.Xattrs["security.capability"] != "cap" {
		t.Fatalf("/bin/g xattrs = %#v, want only security.capability", g.Xattrs)
	}
}

// TestTransformFiltersRunOrphanedHardlinkPayloadXattr is the /run variant: the
// payload lives under /run and is dropped from the tree when /run is emptied,
// yet a hardlink outside /run keeps it and its xattrs must still be filtered.
func TestTransformFiltersRunOrphanedHardlinkPayloadXattr(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "run", typ: tar.TypeDir, mode: 0o755},
		tarEntry{
			name: "run/x", typ: tar.TypeReg, mode: 0o644, body: "data",
			pax: map[string]string{"SCHILY.xattr.security.capability": "cap", "SCHILY.xattr.user.foo": "bar"},
		},
		tarEntry{name: "keep", typ: tar.TypeLink, link: "run/x"},
	)
	entries, canonical := buildInline(t, layer)
	if !canonical {
		t.Fatal("hardlink-into-run image reported non-canonical")
	}
	byPath := entriesByPath(entries)
	if _, ok := byPath["/run/x"]; ok {
		t.Fatal("/run/x survived /run canonicalization")
	}
	keep := byPath["/keep"]
	if string(keep.Content) != "data" {
		t.Fatalf("/keep = %#v, want the surviving payload", keep)
	}
	if len(keep.Xattrs) != 1 || keep.Xattrs["security.capability"] != "cap" {
		t.Fatalf("/keep xattrs = %#v, want only security.capability", keep.Xattrs)
	}
}

// TestTransformStreamOrderParentDiverges asserts a layer that names a child
// before its parent is non-canonical even though the parent is present by the
// end: upstream errors at insert time, so the reordering diverges.
func TestTransformStreamOrderParentDiverges(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a/b", typ: tar.TypeReg, mode: 0o644, body: "x"},
		tarEntry{name: "a", typ: tar.TypeDir, mode: 0o755},
	)
	entries, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("child-before-parent ordering reported canonical")
	}
	byPath := entriesByPath(entries)
	if n, ok := byPath["/a"]; !ok || n.Mode&os.ModeDir == 0 {
		t.Fatalf("/a = %#v, want a directory", n)
	}
	if _, ok := byPath["/a/b"]; !ok {
		t.Fatal("/a/b missing")
	}
}

// TestTransformStreamOrderParentFirstCanonical is the control: the same entries
// in parent-first order have every parent present at insert time, so the image
// stays canonical.
func TestTransformStreamOrderParentFirstCanonical(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a/b", typ: tar.TypeReg, mode: 0o644, body: "x"},
	)
	_, canonical := buildInline(t, layer)
	if !canonical {
		t.Fatal("parent-first ordering reported non-canonical")
	}
}

// TestTransformOpaqueMarkerAbsentParentDiverges asserts an opaque marker whose
// directory does not exist at marker time is a divergence: upstream resolves the
// parent chain before clearing and errors when it is missing.
func TestTransformOpaqueMarkerAbsentParentDiverges(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "ghost/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
	)
	_, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("opaque marker under an absent directory reported canonical")
	}
}

// TestTransformDumpGoldenRootAndRun pins the observable dump for a canonical
// fixture: exactly one "/" row carrying /usr's mtime and only the allowlisted
// xattr, and a "/run" row that is present, empty, and carries the source mtime.
func TestTransformDumpGoldenRootAndRun(t *testing.T) {
	usrMtime := time.Unix(1710000000, 42)
	const wantMtime = "1710000000.000000042"
	layer := tarBytes(t,
		tarEntry{
			name: "usr", typ: tar.TypeDir, mode: 0o755, mtime: usrMtime,
			pax: map[string]string{"SCHILY.xattr.security.capability": "cap", "SCHILY.xattr.user.foo": "bar"},
		},
		tarEntry{name: "run", typ: tar.TypeDir, mode: 0o755, mtime: time.Unix(1600000000, 0)},
		tarEntry{name: "run/child", typ: tar.TypeReg, mode: 0o644, body: "x"},
	)
	entries, canonical := buildInline(t, layer)
	if !canonical {
		t.Fatal("golden fixture reported non-canonical")
	}
	var dump bytes.Buffer
	if err := composefs.WriteDump(&dump, entries); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}

	var rootLine, runLine string
	rootRows := 0
	for _, line := range strings.Split(strings.TrimRight(dump.String(), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case fields[0] == "/":
			rootRows++
			rootLine = line
		case fields[0] == "/run":
			runLine = line
		case strings.HasPrefix(fields[0], "/run/"):
			t.Fatalf("/run has a surviving child row: %q", line)
		}
	}
	if rootRows != 1 {
		t.Fatalf("root rows = %d, want exactly 1\n%s", rootRows, dump.String())
	}

	// PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST [xattrs...]
	rootFields := strings.Fields(rootLine)
	if rootFields[2] != "40755" || rootFields[7] != wantMtime {
		t.Fatalf("root row = %q, want dir mode 40755 and /usr mtime %s", rootLine, wantMtime)
	}
	if !strings.Contains(rootLine, "security.capability=cap") || strings.Contains(rootLine, "user.foo") {
		t.Fatalf("root row xattrs = %q, want only security.capability", rootLine)
	}
	if runLine == "" {
		t.Fatalf("no /run row emitted\n%s", dump.String())
	}
	runFields := strings.Fields(runLine)
	if runFields[2] != "40755" || runFields[7] != wantMtime {
		t.Fatalf("/run row = %q, want dir mode 40755 and source mtime %s", runLine, wantMtime)
	}
}

func TestBuild(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	store := newVerityStore(t)
	layer := tarBytes(t, tarEntry{name: "hello", typ: tar.TypeReg, mode: 0o644, body: "world"})
	img, err := Assemble(context.Background(), store, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
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

// scanMerge scans layer at the builder's next index and merges it: the
// sequential, one-layer-at-a-time build.
func scanMerge(ctx context.Context, b *Builder, layer io.Reader, diffID digest.Digest) error {
	l, err := b.Scan(ctx, layer, diffID, b.next)
	if err != nil {
		return err
	}
	return b.Merge(l)
}

// TestBuilderFreshContextPerScan asserts the per-layer build reproduces the
// whole-image path exactly when every Scan runs under its own context and the
// previous call's context is canceled before the next call: nothing a Scan
// starts outlives it, so a dead context cannot reach into a later layer.
func TestBuilderFreshContextPerScan(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	layers, diffIDs := writerFixture(t)
	want, err := Assemble(context.Background(), newVerityStore(t), openTestLayers(t, layers...), diffIDs)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	b := NewBuilder(newVerityStore(t))
	defer b.Close()
	cancelPrev := func() {}
	for i, layer := range layers {
		cancelPrev()
		ctx, cancel := context.WithCancel(context.Background())
		cancelPrev = cancel
		if err := scanMerge(ctx, b, bytes.NewReader(layer), diffIDs[i]); err != nil {
			t.Fatalf("Scan/Merge layer %d: %v", i, err)
		}
	}
	cancelPrev()
	got, err := b.Finish(context.Background())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got.Descriptor.Digest != want.Descriptor.Digest || got.Descriptor.Size != want.Descriptor.Size ||
		got.FsverityDigest != want.FsverityDigest || got.RootfsSize != want.RootfsSize || got.Canonical != want.Canonical {
		t.Fatalf("Builder image = %#v, want Assemble's %#v", got, want)
	}
}

// TestScanFailureLeavesBuilderUsable asserts a failed Scan touches no builder
// state — the same layer scans and merges afterwards — and leaves no temp
// behind.
func TestScanFailureLeavesBuilderUsable(t *testing.T) {
	store := newVerityStore(t)
	layer := tarBytes(t, tarEntry{name: "f", typ: tar.TypeReg, mode: 0o644, body: large("f")})
	b := NewBuilder(store)
	defer b.Close()
	err := scanMerge(context.Background(), b, bytes.NewReader(layer), digest.FromBytes([]byte("not this layer")))
	skipIfNoVerity(t, err)
	if err == nil || !strings.Contains(err.Error(), "diff_id mismatch") {
		t.Fatalf("Scan error = %v, want diff_id mismatch", err)
	}
	temps, err := os.ReadDir(store.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temps left after failed Scan: %d", len(temps))
	}
	if err := scanMerge(context.Background(), b, bytes.NewReader(layer), digest.FromBytes(layer)); err != nil {
		t.Fatalf("Scan/Merge after a failed Scan = %v, want success", err)
	}
	if b.next != 1 {
		t.Fatalf("next = %d after one merge, want 1", b.next)
	}
}

// TestAssembleJoinsLayerCloseError asserts a layer reader whose Close fails
// has that failure in Assemble's result: alone when the layer applied cleanly,
// joined with the apply failure otherwise. The layer holds only a directory,
// so no object is written and a plain store suffices.
func TestAssembleJoinsLayerCloseError(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "dir", typ: tar.TypeDir, mode: 0o755})
	tests := []struct {
		name      string
		diffID    digest.Digest
		wantApply string
	}{
		{"clean apply", digest.FromBytes(layer), ""},
		{"failed apply", digest.FromBytes([]byte("not this layer")), "diff_id mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := objectstore.New(t.TempDir())
			if err != nil {
				t.Fatalf("objectstore.New: %v", err)
			}
			openLayer := func(int) (io.ReadCloser, error) {
				return failingCloser{Reader: bytes.NewReader(layer)}, nil
			}
			_, err = Assemble(context.Background(), store, openLayer, []digest.Digest{tt.diffID})
			if !errors.Is(err, errCloseSentinel) {
				t.Fatalf("Assemble error = %v, want the close sentinel", err)
			}
			if tt.wantApply != "" && !strings.Contains(err.Error(), tt.wantApply) {
				t.Fatalf("Assemble error = %v, want it to also report %q", err, tt.wantApply)
			}
		})
	}
}

func TestBuildEntriesOpensConsumesClosesLayerBeforeNext(t *testing.T) {
	layers := [][]byte{
		tarBytes(t, tarEntry{name: "one", typ: tar.TypeDir, mode: 0o755}),
		tarBytes(t, tarEntry{name: "two", typ: tar.TypeDir, mode: 0o755}),
		tarBytes(t, tarEntry{name: "three", typ: tar.TypeDir, mode: 0o755}),
	}
	diffIDs := []digest.Digest{
		digest.FromBytes(layers[0]),
		digest.FromBytes(layers[1]),
		digest.FromBytes(layers[2]),
	}

	var events []string
	layerOpen := false
	openLayer := func(i int) (io.ReadCloser, error) {
		if layerOpen {
			t.Fatalf("opened layer %d before closing previous layer", i)
		}
		events = append(events, fmt.Sprintf("open:%d", i))
		layerOpen = true
		return &observedLayer{
			r:      bytes.NewReader(layers[i]),
			index:  i,
			events: &events,
			open:   &layerOpen,
		}, nil
	}

	if _, _, err := BuildEntries(context.Background(), nil, openLayer, diffIDs); err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	if layerOpen {
		t.Fatal("last layer was not closed")
	}

	want := strings.Join([]string{
		"open:0", "eof:0", "close:0:true",
		"open:1", "eof:1", "close:1:true",
		"open:2", "eof:2", "close:2:true",
	}, ",")
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

// large returns a body over the inline threshold, so a file carrying it is
// stored by digest in the CAS rather than inlined into the EROFS metadata.
func large(label string) string {
	return label + ": " + strings.Repeat("padding ", 8)
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

func assertObjectAbsent(t *testing.T, store *objectstore.Store, d digest.Digest) {
	t.Helper()
	p, err := store.ObjectPath(d)
	if err != nil {
		t.Fatalf("ObjectPath(%s): %v", d, err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object %s should be absent (inlined), stat err = %v", d, err)
	}
}

func assertObjectPresent(t *testing.T, store *objectstore.Store, d digest.Digest) {
	t.Helper()
	p, err := store.ObjectPath(d)
	if err != nil {
		t.Fatalf("ObjectPath(%s): %v", d, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("object %s should be stored: %v", d, err)
	}
}

// buildInline merges layers whose regular files all fit the inline threshold, so
// no CAS store is needed, and returns the entries and the canonical flag.
func buildInline(t *testing.T, layers ...[]byte) ([]composefs.Entry, bool) {
	t.Helper()
	diffIDs := make([]digest.Digest, len(layers))
	for i, l := range layers {
		diffIDs[i] = digest.FromBytes(l)
	}
	entries, canonical, err := BuildEntries(context.Background(), nil, openTestLayers(t, layers...), diffIDs)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	return entries, canonical
}

func entriesByPath(entries []composefs.Entry) map[string]composefs.Entry {
	out := make(map[string]composefs.Entry, len(entries))
	for _, e := range entries {
		out[e.Path] = e
	}
	return out
}

type observedLayer struct {
	r        *bytes.Reader
	index    int
	events   *[]string
	open     *bool
	consumed bool
}

func (l *observedLayer) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if errors.Is(err, io.EOF) && !l.consumed {
		*l.events = append(*l.events, fmt.Sprintf("eof:%d", l.index))
		l.consumed = true
	}
	return n, err
}

func (l *observedLayer) Close() error {
	*l.events = append(*l.events, fmt.Sprintf("close:%d:%t", l.index, l.consumed))
	*l.open = false
	return nil
}

// errCloseSentinel is the failure failingCloser reports from Close.
var errCloseSentinel = errors.New("close sentinel")

// failingCloser serves a layer whose Close fails with errCloseSentinel.
type failingCloser struct{ io.Reader }

func (failingCloser) Close() error { return errCloseSentinel }

func openTestLayers(t *testing.T, layers ...[]byte) func(int) (io.ReadCloser, error) {
	t.Helper()
	return func(i int) (io.ReadCloser, error) {
		if i < 0 || i >= len(layers) {
			t.Fatalf("layer index %d out of range", i)
		}
		return io.NopCloser(bytes.NewReader(layers[i])), nil
	}
}

type tarEntry struct {
	name  string
	typ   byte
	mode  int64
	uid   int
	gid   int
	body  string
	link  string
	pax   map[string]string
	mtime time.Time
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
			Uid:        e.uid,
			Gid:        e.gid,
			Linkname:   e.link,
			Size:       int64(len(e.body)),
			PAXRecords: e.pax,
			ModTime:    e.mtime,
		}
		// USTAR cannot encode a sub-second or pre-epoch mtime; PAX can.
		if !e.mtime.IsZero() && (e.mtime.Nanosecond() != 0 || e.mtime.Unix() < 0) {
			hdr.Format = tar.FormatPAX
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

// TestTransformNonDirectoryParentDiverges asserts a name inserted while its
// parent exists as a regular file is a divergence even when a later directory
// entry replaces that file: upstream's parent walk rejects a non-directory
// component at insert time.
func TestTransformNonDirectoryParentDiverges(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a", typ: tar.TypeReg, mode: 0o644, body: "f"},
		tarEntry{name: "a/b", typ: tar.TypeReg, mode: 0o644, body: "x"},
		tarEntry{name: "a", typ: tar.TypeDir, mode: 0o755},
	)
	_, canonical := buildInline(t, layer)
	if canonical {
		t.Fatal("non-directory parent at insert time reported canonical")
	}
}

// writerFixture is a multi-layer image exercising every way the merge can drop
// a node whose object is still queued for sealing — a whiteout, an opaque
// directory, a last-writer replacement — plus a hardlink whose target name is
// whited out, so its payload survives only through the link.
func writerFixture(t *testing.T) ([][]byte, []digest.Digest) {
	t.Helper()
	layer1 := tarBytes(t,
		tarEntry{name: "bin", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: large("app")},
		tarEntry{name: "bin/alias", typ: tar.TypeLink, link: "bin/app"},
		tarEntry{name: "bin/gone", typ: tar.TypeReg, mode: 0o644, body: large("gone")},
		tarEntry{name: "bin/replaced", typ: tar.TypeReg, mode: 0o644, body: large("old")},
		tarEntry{name: "cache", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "cache/a", typ: tar.TypeReg, mode: 0o644, body: large("cache a")},
		tarEntry{name: "cache/b", typ: tar.TypeReg, mode: 0o644, body: large("cache b")},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "bin/.wh.app", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "bin/.wh.gone", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "bin/replaced", typ: tar.TypeReg, mode: 0o644, body: large("new")},
		tarEntry{name: "cache/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
		tarEntry{name: "cache/c", typ: tar.TypeReg, mode: 0o644, body: large("cache c")},
	)
	return [][]byte{layer1, layer2}, []digest.Digest{digest.FromBytes(layer1), digest.FromBytes(layer2)}
}

// TestBuildEntriesWriterMatchesSynchronousBuild asserts that building through
// a Writer yields exactly the entries the synchronous store path yields — the
// fs-verity digest of a payload reachable only through a hardlink included —
// and that the objects the merge dropped are still sealed and no temp is left.
func TestBuildEntriesWriterMatchesSynchronousBuild(t *testing.T) {
	store := newVerityStore(t)
	layers, diffIDs := writerFixture(t)

	want, _, err := BuildEntries(context.Background(), store, openTestLayers(t, layers...), diffIDs)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("synchronous BuildEntries: %v", err)
	}

	w := store.NewWriter(context.Background())
	got, _, err := BuildEntries(context.Background(), w, openTestLayers(t, layers...), diffIDs)
	if err != nil {
		t.Fatalf("Writer BuildEntries: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Writer entries differ from synchronous entries:\n got %#v\nwant %#v", got, want)
	}
	byPath := entriesByPath(got)
	if _, ok := byPath["/bin/app"]; ok {
		t.Fatal("whiteout did not remove /bin/app")
	}
	alias := byPath["/bin/alias"]
	if alias.Hardlink != "" || alias.Digest != digest.FromBytes([]byte(large("app"))) || alias.Verity == "" {
		t.Fatalf("/bin/alias = %#v, want the surviving payload with its fs-verity digest", alias)
	}
	if got := byPath["/bin/replaced"]; got.Digest != digest.FromBytes([]byte(large("new"))) || got.Verity == "" {
		t.Fatalf("/bin/replaced = %#v, want the layer-2 payload with its fs-verity digest", got)
	}
	// Dropped nodes' objects were still sealed and committed, not leaked.
	for _, body := range []string{large("gone"), large("old"), large("cache a"), large("cache b")} {
		if ok, err := store.Has(digest.FromBytes([]byte(body))); err != nil || !ok {
			t.Fatalf("dropped object %q: Has = %v, %v; want committed", body, ok, err)
		}
	}
	temps, err := os.ReadDir(store.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temps left after Close: %d", len(temps))
	}
}

// TestAssembleWriterErrorSurfacesByName asserts a failure on the sealing
// worker — a tampered object already at the CAS path of a file the layer
// stores — fails Assemble with that error, not with the cancellation it
// triggered, and leaves no temp behind. The poisoned file is the only stored
// object and is whited out by the next layer, so no surviving node carries a
// Sealed and no later WriteObject observes the failure: the error can reach
// Assemble only through the writer's Close on the success path.
func TestAssembleWriterErrorSurfacesByName(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	store := newVerityStore(t)
	content := []byte(large("authentic"))
	seedTamperedObject(t, store, content)

	layer1 := tarBytes(t,
		tarEntry{name: "a", typ: tar.TypeReg, mode: 0o644, body: string(content)},
	)
	layer2 := tarBytes(t,
		tarEntry{name: ".wh.a", typ: tar.TypeReg, mode: 0o000},
	)
	_, err := Assemble(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	if err == nil {
		t.Fatal("Assemble succeeded over a tampered object")
	}
	if errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "fs-verity digest mismatch") {
		t.Fatalf("Assemble error = %v, want the collision mismatch by name", err)
	}
	temps, err := os.ReadDir(store.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temps left after failed Assemble: %d", len(temps))
	}
}

// seedTamperedObject pre-seeds a wrong-bytes, verity-enabled object at the
// CAS path content would commit to, so storing content later collides with
// it and the sealing worker fails on the mismatch.
func seedTamperedObject(t *testing.T, store *objectstore.Store, content []byte) {
	t.Helper()
	finalPath, err := store.ObjectPath(digest.FromBytes(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(finalPath) // #nosec G304 -- store-owned test path
	if err != nil {
		t.Fatal(err)
	}
	verr := verity.Enable(f)
	_ = f.Close()
	skipIfNoVerity(t, verr)
	if verr != nil {
		t.Fatalf("seed verity: %v", verr)
	}
}

// holdEOFUntilSealed serves r and delays its EOF until the store's temp dir is
// empty, i.e. until the worker has finished with every object queued so far,
// so the walk's own end-of-stream failure is guaranteed to land after the
// worker's.
type holdEOFUntilSealed struct {
	r        io.Reader
	store    *objectstore.Store
	deadline time.Time
}

func (h *holdEOFUntilSealed) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if !errors.Is(err, io.EOF) {
		return n, err
	}
	for {
		temps, derr := os.ReadDir(h.store.TempDir())
		if derr != nil {
			return n, derr
		}
		if len(temps) == 0 {
			return n, io.EOF
		}
		if time.Now().After(h.deadline) {
			return n, errors.New("holdEOFUntilSealed: temps still present at deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAssembleReportsWorkerErrorWithLayerError asserts that when the walk
// fails on its own — here a diff_id mismatch at end of stream — after the
// worker has already failed on a tampered collision the walk never observed,
// Assemble reports both errors rather than dropping the worker's.
func TestAssembleReportsWorkerErrorWithLayerError(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	store := newVerityStore(t)
	content := []byte(large("authentic"))
	seedTamperedObject(t, store, content)

	layer := tarBytes(t,
		tarEntry{name: "a", typ: tar.TypeReg, mode: 0o644, body: string(content)},
	)
	openLayer := func(int) (io.ReadCloser, error) {
		return io.NopCloser(&holdEOFUntilSealed{r: bytes.NewReader(layer), store: store, deadline: time.Now().Add(10 * time.Second)}), nil
	}
	wrong := digest.FromBytes([]byte("not this layer"))
	_, err := Assemble(context.Background(), store, openLayer, []digest.Digest{wrong})
	if err == nil {
		t.Fatal("Assemble succeeded")
	}
	for _, want := range []string{"diff_id mismatch", "fs-verity digest mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Assemble error = %v, want it to report %q", err, want)
		}
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("Assemble error reports the induced cancellation: %v", err)
	}
	temps, err := os.ReadDir(store.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temps left after failed Assemble: %d", len(temps))
	}
}

// TestAssembleLayerErrorDiscardsQueue asserts that a layer failing after its
// objects were queued — here a diff_id mismatch — returns with no temp left
// behind, whichever side of the queue owned each temp at the moment of the
// failure. (That queued jobs are discarded rather than sealed is pinned
// deterministically by objectstore's TestWriterCancelDiscardsQueue.)
func TestAssembleLayerErrorDiscardsQueue(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	store := newVerityStore(t)
	if _, sealed, err := store.WriteObject(context.Background(), strings.NewReader("probe")); err != nil {
		skipIfNoVerity(t, err)
		t.Fatalf("WriteObject probe: %v", err)
	} else if _, err := sealed(); err != nil {
		t.Fatal(err)
	}

	var entries []tarEntry
	for i := range 16 {
		entries = append(entries, tarEntry{name: fmt.Sprintf("f%d", i), typ: tar.TypeReg, mode: 0o644, body: large(fmt.Sprintf("file %d", i))})
	}
	layer := tarBytes(t, entries...)
	wrong := digest.FromBytes([]byte("not this layer"))
	_, err := Assemble(context.Background(), store, openTestLayers(t, layer), []digest.Digest{wrong})
	if err == nil || !strings.Contains(err.Error(), "diff_id mismatch") {
		t.Fatalf("Assemble error = %v, want diff_id mismatch", err)
	}
	temps, err := os.ReadDir(store.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temps left after failed Assemble: %d", len(temps))
	}
}

// scanMergeFixtures returns images exercising every merge path — the Writer
// fixture's whiteouts and opaque marker over stored files, hardlink groups
// (to stored and inlined payloads, surviving a whiteout of the target),
// a directory replaced by a file with a whiteout beneath it, an opaque marker
// after same-layer content, and files on both sides of the inline threshold.
func scanMergeFixtures(t *testing.T) map[string][][]byte {
	t.Helper()
	writer, _ := writerFixture(t)
	return map[string][][]byte{
		"writer": writer,
		"hardlink": {
			tarBytes(t,
				tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "bin", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: large("app")},
				tarEntry{name: "bin/alias1", typ: tar.TypeLink, link: "bin/app"},
				tarEntry{name: "bin/alias2", typ: tar.TypeLink, link: "bin/alias1"},
				tarEntry{name: "bin/small", typ: tar.TypeReg, mode: 0o644, body: "tiny"},
				tarEntry{name: "bin/small-alias", typ: tar.TypeLink, link: "bin/small"},
			),
			tarBytes(t,
				tarEntry{name: "bin/.wh.app", typ: tar.TypeReg, mode: 0o000},
				tarEntry{name: "bin/alias1", typ: tar.TypeReg, mode: 0o644, body: large("detached")},
			),
		},
		"whiteout": {
			tarBytes(t,
				tarEntry{name: "d", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "d/x", typ: tar.TypeReg, mode: 0o644, body: large("x")},
				tarEntry{name: "d/sub", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "d/sub/y", typ: tar.TypeReg, mode: 0o644, body: large("y")},
				tarEntry{name: "e", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "e/z", typ: tar.TypeReg, mode: 0o644, body: large("z")},
			),
			tarBytes(t,
				tarEntry{name: "d/.wh.sub", typ: tar.TypeReg, mode: 0o000},
				tarEntry{name: "e", typ: tar.TypeReg, mode: 0o644, body: large("e is a file now")},
			),
		},
		"opaque": {
			tarBytes(t,
				tarEntry{name: "usr", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "d", typ: tar.TypeDir, mode: 0o755},
				tarEntry{name: "d/lower", typ: tar.TypeReg, mode: 0o644, body: large("lower")},
			),
			tarBytes(t,
				tarEntry{name: "d/before", typ: tar.TypeReg, mode: 0o644, body: large("before")},
				tarEntry{name: "d/.wh..wh..opq", typ: tar.TypeReg, mode: 0o000},
				tarEntry{name: "d/after", typ: tar.TypeReg, mode: 0o644, body: large("after")},
			),
		},
		"inline": {
			tarBytes(t,
				tarEntry{name: "empty", typ: tar.TypeReg, mode: 0o644},
				tarEntry{name: "one", typ: tar.TypeReg, mode: 0o644, body: "a"},
				tarEntry{name: "max", typ: tar.TypeReg, mode: 0o644, body: strings.Repeat("b", composefs.InlineContentMax)},
				tarEntry{name: "over", typ: tar.TypeReg, mode: 0o644, body: strings.Repeat("c", composefs.InlineContentMax+1)},
				tarEntry{name: "link", typ: tar.TypeSymlink, mode: 0o777, link: "over"},
				tarEntry{name: "fifo", typ: tar.TypeFifo, mode: 0o600},
			),
		},
	}
}

// TestScanAheadMatchesSequential asserts that scanning every layer up front —
// in reverse index order, so no scan can lean on an earlier merge — and merging
// the scanned layers in order yields exactly what scanning and merging each
// layer in turn yields: the same entries, canonical flag, and composefs image.
func TestScanAheadMatchesSequential(t *testing.T) {
	_, mkcomposefsErr := exec.LookPath("mkcomposefs")
	for name, layers := range scanMergeFixtures(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			diffIDs := make([]digest.Digest, len(layers))
			for i, l := range layers {
				diffIDs[i] = digest.FromBytes(l)
			}

			seqStore := newVerityStore(t)
			sequential := NewBuilder(seqStore)
			defer sequential.Close()
			for i, layer := range layers {
				err := scanMerge(ctx, sequential, bytes.NewReader(layer), diffIDs[i])
				skipIfNoVerity(t, err)
				if err != nil {
					t.Fatalf("sequential Scan/Merge layer %d: %v", i, err)
				}
			}
			want, wantCanonical, err := sequential.m.entries()
			if err != nil {
				t.Fatalf("sequential entries: %v", err)
			}

			scanStore := newVerityStore(t)
			scanned := NewBuilder(scanStore)
			defer scanned.Close()
			scans := make([]*Layer, len(layers))
			for i := len(layers) - 1; i >= 0; i-- {
				scans[i], err = scanned.Scan(ctx, bytes.NewReader(layers[i]), diffIDs[i], i)
				if err != nil {
					t.Fatalf("Scan layer %d: %v", i, err)
				}
			}
			for i, l := range scans {
				if err := scanned.Merge(l); err != nil {
					t.Fatalf("Merge layer %d: %v", i, err)
				}
			}
			got, gotCanonical, err := scanned.m.entries()
			if err != nil {
				t.Fatalf("Scan/Merge entries: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scan-ahead entries differ from sequential:\n got %#v\nwant %#v", got, want)
			}
			if gotCanonical != wantCanonical {
				t.Fatalf("scan-ahead canonical = %t, sequential = %t", gotCanonical, wantCanonical)
			}
			if mkcomposefsErr != nil {
				return
			}
			wantImg, err := storeImage(ctx, seqStore, want, wantCanonical)
			if err != nil {
				t.Fatalf("sequential storeImage: %v", err)
			}
			gotImg, err := storeImage(ctx, scanStore, got, gotCanonical)
			if err != nil {
				t.Fatalf("scan-ahead storeImage: %v", err)
			}
			if !reflect.DeepEqual(gotImg, wantImg) {
				t.Fatalf("scan-ahead image = %#v, sequential = %#v", gotImg, wantImg)
			}
		})
	}
}

// countingSink counts the objects written through it and discards their bytes.
type countingSink struct{ writes int }

func (s *countingSink) WriteObject(_ context.Context, r io.Reader) (digest.Digest, objectstore.Sealed, error) {
	s.writes++
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", nil, err
	}
	return digest.FromString("unused"), objectstore.Resolved("sha256:" + strings.Repeat("0", 64)), nil
}

// TestScanRejectsBeforeNextEntry asserts every check a scan can make without
// the merged tree fails the scan at the offending entry: the large regular
// file that follows is never read, so the sink never sees it and no layer is
// returned.
func TestScanRejectsBeforeNextEntry(t *testing.T) {
	big := tarEntry{name: "big", typ: tar.TypeReg, mode: 0o644, body: large("big")}
	tests := []struct {
		name  string
		first tarEntry
		sink  *countingSink
		want  string
	}{
		{"escaping name", tarEntry{name: "../x", typ: tar.TypeDir, mode: 0o755}, &countingSink{}, "escapes root"},
		{"escaping hardlink target", tarEntry{name: "l", typ: tar.TypeLink, link: "../x"}, &countingSink{}, "hardlink l target"},
		{"empty whiteout target", tarEntry{name: ".wh.", typ: tar.TypeReg, mode: 0o000}, &countingSink{}, "empty whiteout target"},
		{"empty symlink target", tarEntry{name: "s", typ: tar.TypeSymlink, mode: 0o777}, &countingSink{}, "symlink s has empty target"},
		{"unsupported type", tarEntry{name: "z", typ: 'Z', mode: 0o644}, &countingSink{}, "unsupported tar entry type"},
		{"stored file without sink", tarEntry{name: "a", typ: tar.TypeReg, mode: 0o644, body: large("a")}, nil, "regular file requires object sink"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layer := tarBytes(t, tt.first, big)
			var sink ObjectSink
			if tt.sink != nil {
				sink = tt.sink
			}
			l, err := scanLayer(context.Background(), sink, bytes.NewReader(layer), digest.FromBytes(layer), 0)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("scanLayer error = %v, want %q", err, tt.want)
			}
			if l != nil {
				t.Fatalf("scanLayer returned a layer with %d records past the invalid entry", len(l.records))
			}
			if tt.sink != nil && tt.sink.writes != 0 {
				t.Fatalf("sink received %d objects past the invalid entry", tt.sink.writes)
			}
		})
	}
}

// TestScanConcurrentSameObject asserts two layers carrying the same stored
// file can be scanned at once on one store: both scans succeed, both names
// resolve to the same content and fs-verity digests, and the store holds the
// object once — the second commit collided with the first and verified it.
func TestScanConcurrentSameObject(t *testing.T) {
	store := newVerityStore(t)
	body := large("shared")
	layers := [][]byte{
		tarBytes(t, tarEntry{name: "a", typ: tar.TypeReg, mode: 0o644, body: body}),
		tarBytes(t, tarEntry{name: "b", typ: tar.TypeReg, mode: 0o644, body: body}),
	}
	b := NewBuilder(store)
	defer b.Close()

	var wg sync.WaitGroup
	scans := make([]*Layer, len(layers))
	errs := make([]error, len(layers))
	for i, layer := range layers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scans[i], errs[i] = b.Scan(context.Background(), bytes.NewReader(layer), digest.FromBytes(layer), i)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("Scan layer %d: %v", i, err)
		}
	}
	for i, l := range scans {
		if err := b.Merge(l); err != nil {
			t.Fatalf("Merge layer %d: %v", i, err)
		}
	}
	entries, _, err := b.m.entries()
	if err != nil {
		t.Fatal(err)
	}
	byPath := entriesByPath(entries)
	a, bb := byPath["/a"], byPath["/b"]
	if a.Digest != digest.FromBytes([]byte(body)) || a.Verity == "" {
		t.Fatalf("/a = %#v, want the stored payload with its fs-verity digest", a)
	}
	if bb.Digest != a.Digest || bb.Verity != a.Verity {
		t.Fatalf("/b = %#v, want the digests of /a %#v", bb, a)
	}

	objectPath, err := store.ObjectPath(a.Digest)
	if err != nil {
		t.Fatal(err)
	}
	objectsDir := filepath.Dir(filepath.Dir(filepath.Dir(objectPath)))
	inodes := make(map[uint64]bool)
	err = filepath.WalkDir(objectsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		inodes[fi.Sys().(*syscall.Stat_t).Ino] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inodes) != 1 {
		t.Fatalf("store holds %d distinct objects, want the shared one only", len(inodes))
	}
}

// TestMergeOutOfOrderSpends asserts a Merge of a layer other than the next
// expected one is refused and spends the builder: neither the layer that was
// due nor Finish is accepted afterwards.
func TestMergeOutOfOrderSpends(t *testing.T) {
	store, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layers := [][]byte{
		tarBytes(t, tarEntry{name: "a", typ: tar.TypeDir, mode: 0o755}),
		tarBytes(t, tarEntry{name: "b", typ: tar.TypeDir, mode: 0o755}),
	}
	b := NewBuilder(store)
	defer b.Close()
	scans := make([]*Layer, len(layers))
	for i, layer := range layers {
		scans[i], err = b.Scan(context.Background(), bytes.NewReader(layer), digest.FromBytes(layer), i)
		if err != nil {
			t.Fatalf("Scan layer %d: %v", i, err)
		}
	}
	if err := b.Merge(scans[1]); err == nil || !strings.Contains(err.Error(), "expected layer 0") {
		t.Fatalf("Merge of layer 1 first = %v, want an out-of-order error", err)
	}
	if err := b.Merge(scans[0]); !errors.Is(err, errBuilderSpent) {
		t.Fatalf("Merge after an out-of-order Merge = %v, want errBuilderSpent", err)
	}
	if _, err := b.Finish(context.Background()); !errors.Is(err, errBuilderSpent) {
		t.Fatalf("Finish after an out-of-order Merge = %v, want errBuilderSpent", err)
	}
}
