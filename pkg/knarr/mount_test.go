// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/octest"
)

func TestMountFromPulledImageE2E(t *testing.T) {
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

	target := filepath.Join(storeRoot, "mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	mounted := false
	t.Cleanup(func() {
		if !mounted {
			return
		}
		if err := eng.Unmount(context.Background(), target); err != nil {
			t.Errorf("cleanup unmount: %v", err)
		}
	})

	err = eng.Mount(context.Background(), lay.Ref, target)
	if skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	mounted = true

	data, err := os.ReadFile(filepath.Join(target, "etc", "config.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data, []byte("hello from knarr\n")) {
		t.Fatalf("mounted file = %q", data)
	}
	if err := eng.Unmount(context.Background(), target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("target not empty after unmount: %v", entries)
	}
}

func TestMountRefusesBindingFsverityMismatch(t *testing.T) {
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
	tamperBindingDigest(t, octest.BindingPath(storeRoot, lay.Root, lay.Platform), digest.SHA256.FromString("wrong").String())

	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if err == nil {
		t.Fatal("expected mount to fail")
	}
	if !strings.Contains(err.Error(), "fs-verity digest mismatch") {
		t.Fatalf("error = %v, want fs-verity mismatch", err)
	}
}

func tamperBindingDigest(t *testing.T, path, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test binding path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	rootfsDoc, ok := doc["rootfs"].(map[string]any)
	if !ok {
		t.Fatalf("binding rootfs has unexpected shape: %s", data)
	}
	fsverityDoc, ok := rootfsDoc["fsverity"].(map[string]any)
	if !ok {
		t.Fatalf("binding fsverity has unexpected shape: %s", data)
	}
	fsverityDoc["digest"] = replacement
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func skipMountEnvironment(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Operation not permitted") ||
		strings.Contains(msg, "Permission denied")
}

// TestMountUnpulledReturnsErrNotFound: mounting a ref that was never pulled
// must fail with an error matching the exported ErrNotFound, so an embedder
// can branch on it without string matching.
func TestMountUnpulledReturnsErrNotFound(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app@" + digest.SHA256.FromString("never pulled").String()
	err = eng.Mount(context.Background(), ref, t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Mount = %v, want an ErrNotFound match", err)
	}
}

// TestMountMissingObjectReturnsErrNotFound: a binding whose composefs object
// was removed from the store must fail with an ErrNotFound match, not a bare
// stat error.
func TestMountMissingObjectReturnsErrNotFound(t *testing.T) {
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

	img, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	b, err := img.Get(lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("Get binding: %v", err)
	}
	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	path, err := obj.ObjectPath(b.Descriptor.Digest)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove composefs object: %v", err)
	}

	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Mount = %v, want an ErrNotFound match", err)
	}
	if !strings.Contains(err.Error(), "image object missing") {
		t.Fatalf("error = %v, want the missing-object cause preserved", err)
	}
}

// TestUnmountNotMountedReturnsErrNotMounted: unmounting a directory with no
// mount must fail with an ErrNotMounted match so teardown paths can treat it
// as already done.
func TestUnmountNotMountedReturnsErrNotMounted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("umount2 needs privileges")
	}
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.Unmount(context.Background(), t.TempDir())
	if !errors.Is(err, ErrNotMounted) {
		t.Fatalf("Unmount = %v, want an ErrNotMounted match", err)
	}
}

// TestMountMetadataOnlyConsumerOverlayE2E exercises the metadata-only mount
// the way a snapshotter consumes it: mount the verified EROFS metadata alone,
// then assemble a single writable overlay whose data-only lower is the
// store's object directory, and check that file content is served through the
// data plane and that the upper takes writes.
func TestMountMetadataOnlyConsumerOverlayE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

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

	objects, err := eng.ObjectsDir()
	if err != nil {
		t.Fatalf("ObjectsDir: %v", err)
	}
	if want := filepath.Join(storeRoot, "objects", "sha256"); objects != want {
		t.Fatalf("ObjectsDir = %q, want %q", objects, want)
	}

	meta := filepath.Join(storeRoot, "meta")
	merged := filepath.Join(storeRoot, "merged")
	upper := filepath.Join(storeRoot, "upper")
	work := filepath.Join(storeRoot, "work")
	for _, dir := range []string{meta, merged, upper, work} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
	}

	var metaMounted, overlayMounted bool
	t.Cleanup(func() {
		if overlayMounted {
			if err := eng.Unmount(context.Background(), merged); err != nil {
				t.Errorf("cleanup unmount overlay: %v", err)
			}
		}
		if metaMounted {
			if err := eng.Unmount(context.Background(), meta); err != nil {
				t.Errorf("cleanup unmount metadata: %v", err)
			}
		}
	})

	err = eng.Mount(context.Background(), lay.Ref, meta, WithMetadataOnly())
	if skipMountEnvironment(err) {
		t.Skipf("mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	metaMounted = true

	// The metadata mount alone carries no file data; content is served only
	// through an overlay with the object directory as its data-only lower.
	opts := "lowerdir=" + meta + "::" + objects +
		",upperdir=" + upper + ",workdir=" + work +
		",redirect_dir=on,metacopy=on,verity=require"
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		t.Fatalf("overlay mount: %v", err)
	}
	overlayMounted = true

	data, err := os.ReadFile(filepath.Join(merged, "etc", "config.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data, []byte("hello from knarr\n")) {
		t.Fatalf("merged file = %q", data)
	}
	if err := os.WriteFile(filepath.Join(merged, "scratch.txt"), []byte("rw"), 0o644); err != nil {
		t.Fatalf("write through overlay upper: %v", err)
	}
}
