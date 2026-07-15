// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/octest"
)

func newInspectEngine(t *testing.T) Engine {
	t.Helper()
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

func TestInspectRejectsUnresolvableRef(t *testing.T) {
	eng := newInspectEngine(t)
	if _, err := eng.Inspect(context.Background(), "repo/app:latest"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Inspect(unrecorded tag) error = %v, want ErrNotFound", err)
	}
	if _, err := eng.Inspect(context.Background(), "app@sha256:nothex"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Inspect(malformed pinned ref) error = %v, want a digest parse error, not ErrNotFound", err)
	}
}

func TestInspectUnknownImageReportsNotFound(t *testing.T) {
	eng := newInspectEngine(t)
	_, err := eng.Inspect(context.Background(), digest.SHA256.FromString("absent").String())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect of an unpulled image: got %v, want ErrNotFound", err)
	}
}

func TestInspectCanceledContext(t *testing.T) {
	eng := newInspectEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := eng.Inspect(ctx, digest.SHA256.FromString("x").String())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect with a canceled context: got %v, want context.Canceled", err)
	}
}

func TestInspectAfterPullE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	binding := readPullBinding(t, storeRoot, lay.Root, lay.Platform)

	// Inspecting by the full ref and by a bare digest agree and match the image.
	byRef, err := eng.Inspect(context.Background(), lay.Ref)
	if err != nil {
		t.Fatalf("Inspect(ref): %v", err)
	}
	byDigest, err := eng.Inspect(context.Background(), lay.Root.String())
	if err != nil {
		t.Fatalf("Inspect(digest): %v", err)
	}
	if byRef.Root != byDigest.Root || byRef.Composefs.Digest != byDigest.Composefs.Digest {
		t.Fatalf("ref and bare-digest inspect disagree: %#v vs %#v", byRef, byDigest)
	}

	info := byRef
	if info.Root != lay.Root {
		t.Fatalf("Root = %s, want %s", info.Root, lay.Root)
	}
	if info.Platform.OS != lay.Platform.OS || info.Platform.Architecture != lay.Platform.Architecture {
		t.Fatalf("Platform = %#v, want %#v", info.Platform, lay.Platform)
	}
	if len(info.DiffIDs) != len(lay.DiffIDs) {
		t.Fatalf("DiffIDs len = %d, want %d", len(info.DiffIDs), len(lay.DiffIDs))
	}
	for i := range info.DiffIDs {
		if info.DiffIDs[i] != lay.DiffIDs[i] {
			t.Fatalf("DiffIDs[%d] = %s, want %s", i, info.DiffIDs[i], lay.DiffIDs[i])
		}
	}
	if info.Composefs.Digest != binding.RootFS.Descriptor.Digest {
		t.Fatalf("Composefs digest = %s, want %s", info.Composefs.Digest, binding.RootFS.Descriptor.Digest)
	}
	if info.FsverityDigest != binding.RootFS.Fsverity.Digest.String() {
		t.Fatalf("FsverityDigest = %s, want %s", info.FsverityDigest, binding.RootFS.Fsverity.Digest)
	}

	// After the image is removed, Inspect is fail-closed even though the metadata
	// blobs remain in the object store.
	if err := eng.Remove(context.Background(), lay.Ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := eng.Inspect(context.Background(), lay.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect after remove: got %v, want ErrNotFound", err)
	}
}

func TestInspectPlatformMismatch(t *testing.T) {
	storeRoot, obj := seededVerityStore(t)
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(),
		ocispec.Platform{OS: "linux", Architecture: "mips64le"},
		octest.File{Name: "bin/true", Mode: 0o755, Body: "x"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	seedMetadata(t, obj, lay)

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = eng.Inspect(context.Background(), lay.Root.String())
	if err == nil {
		t.Fatal("expected a platform-mismatch error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("platform mismatch should not be ErrNotFound: %v", err)
	}
}

func TestInspectCorruptMetadata(t *testing.T) {
	storeRoot, obj := seededVerityStore(t)
	garbage := []byte("not json")
	d := digest.SHA256.FromBytes(garbage)
	err := obj.WriteBlob(context.Background(), d, bytes.NewReader(garbage))
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = eng.Inspect(context.Background(), d.String())
	if err == nil {
		t.Fatal("expected a metadata parse error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt metadata should not be ErrNotFound: %v", err)
	}
}

// seededVerityStore returns a verity-capable store root and an object store on
// it, skipping when fs-verity is unavailable.
func seededVerityStore(t *testing.T) (string, *objectstore.Store) {
	t.Helper()
	root, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	obj, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	return root, obj
}

// seedMetadata copies a layout's root, manifest, and config blobs into the
// object store so a non-pulled (e.g. non-host-platform) image can be inspected.
func seedMetadata(t *testing.T, obj *objectstore.Store, lay octest.Layout) {
	t.Helper()
	for _, d := range []digest.Digest{lay.Root, lay.Manifest, lay.Config} {
		data, err := os.ReadFile(filepath.Join(lay.Dir, "blobs", "sha256", d.Hex())) // #nosec G304 -- test path from a digest it wrote
		if err != nil {
			t.Fatalf("read blob %s: %v", d, err)
		}
		err = obj.WriteBlob(context.Background(), d, bytes.NewReader(data))
		if devtest.IsVerityUnsupported(err) {
			t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
		}
		if err != nil {
			t.Fatalf("seed blob %s: %v", d, err)
		}
	}
}
