// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/octest"
	"github.com/foundriesio/knarr/internal/refstore"
)

func TestEngineList(t *testing.T) {
	root := t.TempDir()
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	empty, err := eng.List(context.Background())
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty list len = %d, want 0", len(empty))
	}

	firstRoot := digest.SHA256.FromString("first")
	secondRoot := digest.SHA256.FromString("second")
	putEngineRef(t, root, "z@"+secondRoot.String(), secondRoot, 20, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	putEngineRef(t, root, "a@"+firstRoot.String(), firstRoot, 10, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	putEngineBinding(t, root, secondRoot, ocispec.Platform{OS: "linux", Architecture: "arm64"}, digest.SHA256.FromString("cfs-second"))
	putEngineBinding(t, root, firstRoot, ocispec.Platform{OS: "linux", Architecture: "amd64"}, digest.SHA256.FromString("cfs-first"))

	got, err := eng.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Size is the binding's composefs object size (putEngineBinding writes 123),
	// not the ref record's root descriptor size.
	if got[0] != (Image{Ref: "a@" + firstRoot.String(), Digest: firstRoot.String(), Size: 123}) {
		t.Fatalf("first image = %#v", got[0])
	}
	if got[1] != (Image{Ref: "z@" + secondRoot.String(), Digest: secondRoot.String(), Size: 123}) {
		t.Fatalf("second image = %#v", got[1])
	}
}

func TestEngineRemoveDeletesRefAndBindingOnly(t *testing.T) {
	root := t.TempDir()
	imageRoot := digest.SHA256.FromString("root")
	composefs := digest.SHA256.FromString("composefs")
	ref := "registry.example.com/app@" + imageRoot.String()
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	putEngineRef(t, root, ref, imageRoot, 45, platform)
	putEngineBinding(t, root, imageRoot, platform, composefs)
	objectPath := writeEngineObject(t, root, composefs)

	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := eng.Remove(context.Background(), ref, WithForce()); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	refStore := newRefStore(t, root)
	if _, err := refStore.Get(ref); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("Get ref error = %v, want ErrNotFound", err)
	}
	imgStore := newImageStore(t, root)
	if _, err := imgStore.Get(imageRoot, platform); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("Get binding error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("object was removed: %v", err)
	}
}

func TestEngineRemoveKeepsSharedBinding(t *testing.T) {
	root := t.TempDir()
	imageRoot := digest.SHA256.FromString("root")
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	firstRef := "registry.example.com/first@" + imageRoot.String()
	secondRef := "registry.example.com/second@" + imageRoot.String()
	putEngineRef(t, root, firstRef, imageRoot, 45, platform)
	putEngineRef(t, root, secondRef, imageRoot, 45, platform)
	putEngineBinding(t, root, imageRoot, platform, digest.SHA256.FromString("composefs"))

	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := eng.Remove(context.Background(), firstRef); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	refStore := newRefStore(t, root)
	if _, err := refStore.Get(firstRef); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("Get first ref error = %v, want ErrNotFound", err)
	}
	if _, err := refStore.Get(secondRef); err != nil {
		t.Fatalf("second ref missing: %v", err)
	}
	if _, err := newImageStore(t, root).Get(imageRoot, platform); err != nil {
		t.Fatalf("shared binding removed: %v", err)
	}
}

func TestEngineRemoveUnknownRef(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.Remove(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("error = %v, want an ErrNotFound match", err)
	}
}

func TestEngineRemoveRejectsMalformedPlatform(t *testing.T) {
	root := t.TempDir()
	imageRoot := digest.SHA256.FromString("root")
	refStore := newRefStore(t, root)
	if err := refStore.Put("registry.example.com/app@"+imageRoot.String(), ocispec.Descriptor{
		Digest: imageRoot,
		Size:   45,
	}, time.Unix(0, 0).UTC(), []string{"linux"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.Remove(context.Background(), "registry.example.com/app@"+imageRoot.String())
	if err == nil || !strings.Contains(err.Error(), "invalid platform") {
		t.Fatalf("error = %v, want invalid platform", err)
	}
}

func TestPullListRemoveE2E(t *testing.T) {
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
	if _, err := exec.LookPath("mount.composefs"); err != nil {
		t.Skip("mount.composefs not found in PATH")
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

	images, err := eng.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	binding := readPullBinding(t, storeRoot, lay.Root, lay.Platform)
	wantRef := mustNormalizeRef(t, lay.Ref)
	if len(images) != 1 || images[0].Ref != wantRef || images[0].Digest != lay.Root.String() ||
		images[0].Size != binding.RootFS.Descriptor.Size {
		t.Fatalf("List = %#v, want pulled ref/root and the composefs object size", images)
	}

	if err := eng.Remove(context.Background(), lay.Ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	images, err = eng.List(context.Background())
	if err != nil {
		t.Fatalf("List after remove: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("List after remove = %#v, want empty", images)
	}
	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not pulled") {
		t.Fatalf("Mount after remove error = %v, want not pulled", err)
	}

	assertObjectExists(t, storeRoot, lay.Root)
	assertObjectExists(t, storeRoot, lay.Manifest)
	assertObjectExists(t, storeRoot, lay.Config)
	assertObjectExists(t, storeRoot, lay.FileDigest)
	assertObjectExists(t, storeRoot, binding.RootFS.Descriptor.Digest)
}

func putEngineRef(t *testing.T, root, ref string, d digest.Digest, size int64, p ocispec.Platform) {
	t.Helper()
	refStore := newRefStore(t, root)
	if err := refStore.Put(ref, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    d,
		Size:      size,
	}, time.Unix(0, 0).UTC(), []string{ocilayout.PlatformString(&p)}); err != nil {
		t.Fatalf("Put ref: %v", err)
	}
}

func putEngineBinding(t *testing.T, root string, d digest.Digest, p ocispec.Platform, composefs digest.Digest) {
	t.Helper()
	imgStore := newImageStore(t, root)
	if err := imgStore.Put(d, p, imagestore.Binding{
		Descriptor: ocispec.Descriptor{
			MediaType: "application/vnd.knarr.composefs.image.v1+erofs",
			Digest:    composefs,
			Size:      123,
		},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put binding: %v", err)
	}
}

func writeEngineObject(t *testing.T, root string, d digest.Digest) string {
	t.Helper()
	path := octest.ObjectPath(root, d)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("object"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func newRefStore(t *testing.T, root string) *refstore.Store {
	t.Helper()
	refStore, err := refstore.New(root)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	return refStore
}

func newImageStore(t *testing.T, root string) *imagestore.Store {
	t.Helper()
	imgStore, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	return imgStore
}

// TestEngineListToleratesMissingBinding: a valid ref whose binding is absent
// must not brick the listing — the image is listed with a zero size while
// healthy images keep their composefs object size.
func TestEngineListToleratesMissingBinding(t *testing.T) {
	root := t.TempDir()
	healthyRoot := digest.SHA256.FromString("healthy")
	orphanRoot := digest.SHA256.FromString("orphan")
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	putEngineRef(t, root, "healthy@"+healthyRoot.String(), healthyRoot, 45, platform)
	putEngineBinding(t, root, healthyRoot, platform, digest.SHA256.FromString("composefs"))
	putEngineRef(t, root, "orphan@"+orphanRoot.String(), orphanRoot, 45, platform)

	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	images, err := eng.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("len = %d, want 2 (missing binding must not hide images)", len(images))
	}
	if images[0].Ref != "healthy@"+healthyRoot.String() || images[0].Size != 123 {
		t.Fatalf("healthy image = %#v, want size 123", images[0])
	}
	if images[1].Ref != "orphan@"+orphanRoot.String() || images[1].Size != 0 {
		t.Fatalf("orphan image = %#v, want size 0", images[1])
	}
}
