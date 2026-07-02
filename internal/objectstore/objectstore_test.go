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
