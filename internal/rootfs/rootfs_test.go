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
	_, err := buildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected escaping path error")
	}
}

func TestBuildEntriesHardlinkToSymlink(t *testing.T) {
	layer := tarBytes(t,
		tarEntry{name: "target", typ: tar.TypeSymlink, mode: 0o777, link: "somewhere"},
		tarEntry{name: "link", typ: tar.TypeLink, link: "target"},
	)
	entries, err := buildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
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
	_, err := buildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{digest.FromBytes(layer)})
	if err == nil {
		t.Fatal("expected missing hardlink target error")
	}
}

func TestBuildEntriesChecksDiffID(t *testing.T) {
	layer := tarBytes(t, tarEntry{name: "dir", typ: tar.TypeDir, mode: 0o755})
	wrong := digest.SHA256.FromString("wrong")
	_, err := buildEntries(context.Background(), nil, openTestLayers(t, layer), []digest.Digest{wrong})
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
	entries, err := buildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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
	// "/app-copy" sorts before "/usr/bin/app", so it carries the shared inode's
	// payload and the original name is emitted as the hardlink entry.
	appCopy := byPath["/app-copy"]
	if appCopy.Digest != digest.FromBytes([]byte("app")) || appCopy.Verity == "" || appCopy.Hardlink != "" || appCopy.Nlink != 2 {
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
	entries, err := buildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: "app"},
		tarEntry{name: "bin/alias1", typ: tar.TypeLink, link: "bin/app"},
		tarEntry{name: "bin/alias2", typ: tar.TypeLink, link: "bin/alias1"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "bin/.wh.app", typ: tar.TypeReg, mode: 0o000},
	)
	entries, err := buildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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
	if got := byPath["/bin/alias1"]; got.Digest != digest.FromBytes([]byte("app")) || got.Mode.Perm() != 0o755 || got.Hardlink != "" || got.Nlink != 2 {
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
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: "v1"},
		tarEntry{name: "bin/alias", typ: tar.TypeLink, link: "bin/app"},
	)
	layer2 := tarBytes(t,
		tarEntry{name: "bin/app", typ: tar.TypeReg, mode: 0o755, body: "v2"},
	)
	entries, err := buildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
		digest.FromBytes(layer1),
		digest.FromBytes(layer2),
	})
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("buildEntries: %v", err)
	}
	byPath := entriesByPath(entries)
	if got := byPath["/bin/app"]; got.Digest != digest.FromBytes([]byte("v2")) || got.Hardlink != "" {
		t.Fatalf("/bin/app = %#v, want the replacing v2 payload", got)
	}
	if got := byPath["/bin/alias"]; got.Digest != digest.FromBytes([]byte("v1")) || got.Hardlink != "" {
		t.Fatalf("/bin/alias = %#v, want the original v1 payload", got)
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
	entries, err := buildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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
	entries, err := buildEntries(context.Background(), store, openTestLayers(t, layer1, layer2), []digest.Digest{
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

	if _, err := buildEntries(context.Background(), nil, openLayer, diffIDs); err != nil {
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
