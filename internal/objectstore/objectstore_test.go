// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/verity"
)

// newTestStore opens a store on a plain temp dir, for paths that do not exercise
// fs-verity (metadata blobs).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// newVerityStore opens a store on the dev container's verity-capable filesystem,
// for the object paths that enable fs-verity. It skips when that filesystem is
// unavailable (running outside the privileged dev container).
func newVerityStore(t *testing.T) *Store {
	t.Helper()
	root, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// skipIfNoVerity skips the test when err reports that the test filesystem does
// not support fs-verity, so the verity-dependent paths stay meaningful only where
// fs-verity is actually available.
func skipIfNoVerity(t *testing.T, err error) {
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
}

func TestWriteObjectRoundTrip(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := []byte("hello knarr object")

	d, fsv, err := s.WriteObject(ctx, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	if want := digest.FromBytes(content); d != want {
		t.Fatalf("digest = %s, want %s", d, want)
	}
	// WriteObject returns the measured fs-verity digest; it must agree with a
	// fresh measurement of the committed object.
	measured, err := s.VerityDigest(d)
	if err != nil {
		t.Fatalf("VerityDigest: %v", err)
	}
	if fsv == "" || fsv != measured {
		t.Fatalf("WriteObject fs-verity digest = %q, VerityDigest = %q", fsv, measured)
	}

	// Re-writing identical content dedups via the collision path and still
	// reports the existing object's fs-verity digest.
	d2, fsv2, err := s.WriteObject(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("WriteObject dedup: %v", err)
	}
	if d2 != d {
		t.Fatalf("dedup digest = %s, want %s", d2, d)
	}
	if fsv2 != measured {
		t.Fatalf("dedup fs-verity digest = %q, want %q", fsv2, measured)
	}
}

// sameInode fails the test unless the object and view names resolve to one inode.
func sameInode(t *testing.T, objPath, viewPath string) {
	t.Helper()
	objFI, err := os.Lstat(objPath)
	if err != nil {
		t.Fatalf("lstat object %s: %v", objPath, err)
	}
	viewFI, err := os.Lstat(viewPath)
	if err != nil {
		t.Fatalf("lstat view %s: %v", viewPath, err)
	}
	if !os.SameFile(objFI, viewFI) {
		t.Fatalf("view %s and object %s are not the same inode", viewPath, objPath)
	}
}

// TestWriteObjectLinksView asserts a committed object is also reachable under the
// fs-verity view, as a hardlink to the same inode.
func TestWriteObjectLinksView(t *testing.T) {
	s := newVerityStore(t)
	content := []byte("view-linked object")

	d, fsv, err := s.WriteObject(context.Background(), bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	sameInode(t, s.objectPath(d.Hex()), s.viewPath(strings.TrimPrefix(fsv, "sha256:")))
}

// TestWriteBlobLinksView asserts metadata blobs are view-linked too: every sealed
// object gets a view name.
func TestWriteBlobLinksView(t *testing.T) {
	s := newVerityStore(t)
	content := []byte(`{"schemaVersion":2,"view":true}`)
	d := digest.FromBytes(content)

	err := s.WriteBlob(context.Background(), d, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	fsv, err := s.VerityDigest(d)
	if err != nil {
		t.Fatalf("VerityDigest: %v", err)
	}
	sameInode(t, s.objectPath(d.Hex()), s.viewPath(strings.TrimPrefix(fsv, "sha256:")))
}

// TestWriteObjectRepairsView removes a committed object's view link, then rewrites
// identical content: the collision path must repair the missing view name.
func TestWriteObjectRepairsView(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := []byte("repair my view link")

	d, fsv, err := s.WriteObject(ctx, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	viewPath := s.viewPath(strings.TrimPrefix(fsv, "sha256:"))
	if err := os.Remove(viewPath); err != nil {
		t.Fatalf("remove view link: %v", err)
	}
	// Identical content hits the collision path, which repairs the view link.
	if _, _, err := s.WriteObject(ctx, bytes.NewReader(content)); err != nil {
		t.Fatalf("WriteObject repair: %v", err)
	}
	sameInode(t, s.objectPath(d.Hex()), viewPath)
}

// TestWriteObjectViewMismatch seeds a foreign file at the view name (with the CAS
// object dropped so the write takes the new-commit path) and asserts the view
// link's same-inode check rejects it as corruption.
func TestWriteObjectViewMismatch(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := []byte("mismatch content")

	d, fsv, err := s.WriteObject(ctx, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	// Drop the CAS object so the next write commits fresh, and replace the view
	// name with a foreign file so the view link collides with a different inode.
	if err := os.Remove(s.objectPath(d.Hex())); err != nil {
		t.Fatalf("remove object: %v", err)
	}
	viewPath := s.viewPath(strings.TrimPrefix(fsv, "sha256:"))
	if err := os.Remove(viewPath); err != nil {
		t.Fatalf("remove view link: %v", err)
	}
	if err := os.WriteFile(viewPath, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("seed foreign view: %v", err)
	}
	if _, _, err := s.WriteObject(ctx, bytes.NewReader(content)); err == nil {
		t.Fatal("expected error when the view name resolves to a different inode")
	}
}

// TestWriteObjectConcurrentView races many writers of identical content on a
// target that does not exist yet, so the initial CAS commit, the collision
// path, and view-link creation genuinely interleave; afterward the single
// object and its view name must be one inode. Run under -race.
func TestWriteObjectConcurrentView(t *testing.T) {
	s := newVerityStore(t)

	// Prime with DIFFERENT content only to surface an unsupported-fs skip
	// before spawning goroutines; the raced target stays absent until they run.
	_, _, err := s.WriteObject(context.Background(), bytes.NewReader([]byte("verity probe")))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject prime: %v", err)
	}

	content := []byte("concurrently written content")
	const writers = 8
	errs := make([]error, writers)
	fsvs := make([]string, writers)
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			_, fsvs[i], errs[i] = s.WriteObject(context.Background(), bytes.NewReader(content))
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent WriteObject %d: %v", i, e)
		}
	}
	d := digest.FromBytes(content)
	sameInode(t, s.objectPath(d.Hex()), s.viewPath(strings.TrimPrefix(fsvs[0], "sha256:")))
}

// TestWriteObjectCollisionForeignView leaves the CAS object intact but replaces
// its view name with a foreign inode: the collision verify succeeds, and the
// view repair must then reject the mismatched link as corruption.
func TestWriteObjectCollisionForeignView(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := []byte("collision foreign view")

	_, fsv, err := s.WriteObject(ctx, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	viewPath := s.viewPath(strings.TrimPrefix(fsv, "sha256:"))
	if err := os.Remove(viewPath); err != nil {
		t.Fatalf("remove view link: %v", err)
	}
	if err := os.WriteFile(viewPath, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("seed foreign view: %v", err)
	}
	if _, _, err := s.WriteObject(ctx, bytes.NewReader(content)); err == nil {
		t.Fatal("expected error when the collision path finds a foreign inode at the view name")
	}
}

// TestWriteObjectTamperedCollision pre-seeds a wrong-bytes, verity-enabled
// object at the target path and asserts that writing the authentic content
// fails rather than silently deduping onto the tampered file.
func TestWriteObjectTamperedCollision(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := []byte("authentic content")
	d := digest.FromBytes(content)

	finalPath := s.objectPath(d.Hex())
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("tampered"), 0o400); err != nil {
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

	if _, _, err := s.WriteObject(ctx, bytes.NewReader(content)); err == nil {
		t.Fatal("expected error writing authentic content over a tampered object")
	}
}

// TestCheckVerityRejectsNonVerityFS asserts the preflight fails with an
// actionable error when the store filesystem cannot enable fs-verity (tmpfs).
func TestCheckVerityRejectsNonVerityFS(t *testing.T) {
	s := newTestStore(t) // t.TempDir() is tmpfs, which has no fs-verity
	err := s.CheckVerity()
	if err == nil {
		t.Skip("store filesystem unexpectedly supports fs-verity")
	}
	if !strings.Contains(err.Error(), "fs-verity-capable filesystem") {
		t.Errorf("error should explain the store fs lacks fs-verity: %v", err)
	}
}

func TestWriteBlobRoundTrip(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := []byte(`{"schemaVersion":2}`)
	d := digest.FromBytes(content)

	err := s.WriteBlob(ctx, d, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	// Digest mismatch is rejected.
	if err := s.WriteBlob(ctx, d, bytes.NewReader([]byte("different"))); err == nil {
		t.Fatal("expected digest-mismatch error")
	}
}

func TestObjectPaths(t *testing.T) {
	s := newTestStore(t)
	d := digest.SHA256.FromString("object")

	path, err := s.ObjectPath(d)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	want := filepath.Join(s.root, "objects", "sha256", d.Hex()[:2], d.Hex()[2:])
	if path != want {
		t.Fatalf("ObjectPath = %q, want %q", path, want)
	}
	if got := s.ObjectsDir(); got != filepath.Join(s.root, "objects", "sha256") {
		t.Fatalf("ObjectsDir = %q", got)
	}
	if got := s.ViewDir(); got != filepath.Join(s.root, "objects", "fsverity-sha256-12") {
		t.Fatalf("ViewDir = %q", got)
	}
	if _, err := s.ObjectPath(digest.Digest("sha512:abc")); err == nil {
		t.Fatal("expected invalid digest error")
	}
}

func TestOpenRejectsBadDigest(t *testing.T) {
	s := newTestStore(t)
	tests := []struct {
		name string
		d    digest.Digest
	}{
		{"empty", ""},
		{"non-sha256", digest.Digest("sha512:" + strings.Repeat("a", 128))},
		{"malformed", digest.Digest("sha256:zz")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Open(tt.d); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestOpenMissingObject(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Open(digest.SHA256.FromString("absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open of a missing object: got %v, want os.ErrNotExist", err)
	}
}

func TestHas(t *testing.T) {
	s := newTestStore(t)
	d := digest.SHA256.FromString("object")

	// Absent.
	if has, err := s.Has(d); err != nil || has {
		t.Fatalf("Has absent = (%v, %v), want (false, nil)", has, err)
	}

	path, err := s.ObjectPath(d)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// A directory planted at the object path is not a stored object.
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir at object path: %v", err)
	}
	if has, err := s.Has(d); err != nil || has {
		t.Fatalf("Has of a directory node = (%v, %v), want (false, nil)", has, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove dir: %v", err)
	}

	// A symlink at the object path (even one pointing at a real file) is not a
	// stored object: Lstat judges it on its own type.
	target := filepath.Join(t.TempDir(), "real")
	if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink at object path: %v", err)
	}
	if has, err := s.Has(d); err != nil || has {
		t.Fatalf("Has of a symlink node = (%v, %v), want (false, nil)", has, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove symlink: %v", err)
	}

	// A regular file is present.
	if err := os.WriteFile(path, []byte("object"), 0o600); err != nil {
		t.Fatalf("WriteFile object: %v", err)
	}
	if has, err := s.Has(d); err != nil || !has {
		t.Fatalf("Has of a regular object = (%v, %v), want (true, nil)", has, err)
	}
}

func TestOpenRoundTrip(t *testing.T) {
	s := newVerityStore(t)
	content := []byte("metadata blob bytes")
	d := digest.SHA256.FromBytes(content)
	err := s.WriteBlob(context.Background(), d, bytes.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	rc, err := s.Open(d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read back %q, want %q", got, content)
	}
}
