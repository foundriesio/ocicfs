// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/octest"
	"github.com/foundriesio/ocicfs/internal/verity"
)

// requiredHint is the remedy a required-mode measurement of an object
// without fs-verity names.
const requiredHint = "carries no fs-verity, which this build requires"

// plainStoreRoot returns a store root on a filesystem without fs-verity (the
// test temp dir: tmpfs or overlayfs), skipping when it unexpectedly has it.
func plainStoreRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	obj, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	if obj.CheckVerity() == nil {
		t.Skip("temp dir filesystem unexpectedly supports fs-verity")
	}
	return root
}

// verityState returns the fs-verity state eng runs its store in.
func verityState(t *testing.T, eng Engine) string {
	t.Helper()
	state, err := eng.VerityState()
	if err != nil {
		t.Fatalf("VerityState: %v", err)
	}
	return state
}

// plantPlainObject writes body as an object without fs-verity straight into
// the store's object tree, as an off build writes it, and returns its path.
func plantPlainObject(t *testing.T, root, body string) string {
	t.Helper()
	path := octest.ObjectPath(root, digest.SHA256.FromString(body))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// skipPlainObjects skips a test that seeds objects as plain files, without
// fs-verity, in a build made in required mode, which fails on them wherever
// it measures one.
func skipPlainObjects(t *testing.T) {
	t.Helper()
	if verity.Mode == verity.ModeRequired {
		t.Skip("seeds objects without fs-verity, which a build made in required mode refuses")
	}
}

// pullModeLayout pulls a one-file image from a layout into eng.
func pullModeLayout(t *testing.T, eng Engine, body string) (octest.Layout, error) {
	t.Helper()
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/file.txt", Mode: 0o644, Body: body},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	_, err = eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir))
	return lay, err
}

const modeBody = "content for the fs-verity mode tests, longer than the inline threshold\n"

// TestPullModeWithoutFsverity pins the build modes on a filesystem without
// fs-verity: auto and off pull objects without it, required fails the pull
// and leaves the store untouched.
func TestPullModeWithoutFsverity(t *testing.T) {
	root := plainStoreRoot(t)
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = pullModeLayout(t, eng, modeBody)
	if verity.Mode == verity.ModeRequired {
		if err == nil || !strings.Contains(err.Error(), "fs-verity-capable filesystem") {
			t.Fatalf("Pull = %v, want the fs-verity preflight error", err)
		}
		entries, err := os.ReadDir(filepath.Join(root, "objects", "sha256"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("object tree after a refused pull = %d entries, %v; want none", len(entries), err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got := verityState(t, eng); got != "disabled" {
		t.Fatalf("verity state = %q, want disabled", got)
	}
	required, err := eng.VerityRequired()
	if err != nil || required {
		t.Fatalf("VerityRequired = %v, %v; want false", required, err)
	}
}

// TestPullModeWithFsverity pins the build modes on a filesystem with
// fs-verity: auto and required pull objects with it, off without.
func TestPullModeWithFsverity(t *testing.T) {
	root := verityStoreRoot(t)
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := pullModeLayout(t, eng, modeBody); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	want := "enabled"
	if verity.Mode == verity.ModeOff {
		want = "disabled"
	}
	if got := verityState(t, eng); got != want {
		t.Fatalf("verity state = %q, want %s", got, want)
	}
	required, err := eng.VerityRequired()
	if err != nil || required != (want == "enabled") {
		t.Fatalf("VerityRequired = %v, %v; want %v", required, err, want == "enabled")
	}
	obj, err := os.Open(octest.ObjectPath(root, digest.SHA256.FromString(modeBody))) // #nosec G304 -- test store path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = obj.Close() }()
	if _, err := verity.Measure(obj); (err == nil) != (want == "enabled") {
		t.Fatalf("object measured with fs-verity: %v; want fs-verity exactly when enabled", err)
	}
}

// TestVerityStateUnsupported pins a build made in required mode on a
// filesystem without fs-verity: VerityState reports unsupported, where a
// pull fails, and writes nothing under objects/.
func TestVerityStateUnsupported(t *testing.T) {
	if verity.Mode != verity.ModeRequired {
		t.Skip("only a build made in required mode reports unsupported")
	}
	root := plainStoreRoot(t)
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if state, err := eng.VerityState(); err != nil || state != "unsupported" {
		t.Fatalf("VerityState = %q, %v; want unsupported", state, err)
	}
	for _, dir := range []string{"objects/sha256", "objects/fsverity-sha256-12"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s after VerityState = %d entries, %v; want none", dir, len(entries), err)
		}
	}
}

// TestPullReusesObjectWithFsverity asserts every build mode reuses an object
// with fs-verity the store already holds, and mounts the image over it: in
// off mode the userspace digest of such an object equals its measurement.
func TestPullReusesObjectWithFsverity(t *testing.T) {
	root := verityStoreRoot(t)
	path := plantPlainObject(t, root, modeBody)
	f, err := os.Open(path) // #nosec G304 -- test store path
	if err != nil {
		t.Fatal(err)
	}
	err = verity.Enable(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("enable fs-verity: %v", err)
	}
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lay, err := pullModeLayout(t, eng, modeBody)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	mountAndRead(t, eng, lay.Ref)
}

// TestRequiredModeFailsOnObjectWithoutFsverity pins required mode meeting
// an object written without fs-verity: a pull that would reuse it fails,
// and a mount whose composefs image lost its fs-verity fails, both naming
// the mismatch, and never without fs-verity.
func TestRequiredModeFailsOnObjectWithoutFsverity(t *testing.T) {
	if verity.Mode == verity.ModeOff {
		t.Skip("a build made in off mode requires no fs-verity")
	}
	root := verityStoreRoot(t)
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantPlainObject(t, root, modeBody)
	if _, err := pullModeLayout(t, eng, modeBody); err == nil || !strings.Contains(err.Error(), requiredHint) {
		t.Fatalf("Pull over an object without fs-verity = %v; want the mismatch named", err)
	}

	lay, err := pullModeLayout(t, eng, modeBody+"second image\n")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	info, err := eng.Inspect(context.Background(), lay.Ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Re-create the composefs image object with the same bytes and no
	// fs-verity, as an off build would have written it.
	imagePath := octest.ObjectPath(root, info.Composefs.Digest)
	image, err := os.ReadFile(imagePath) // #nosec G304 -- test store path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, image, 0o600); err != nil { // #nosec G304 G703 -- test store path from the inspected digest
		t.Fatal(err)
	}
	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), requiredHint) {
		t.Fatalf("Mount over an image without fs-verity = %v; want the mismatch named", err)
	}
}

// mountAndRead mounts ref read-only, reads the image's object-backed file
// through the mount, and unmounts it, skipping where mounting is unavailable.
func mountAndRead(t *testing.T, eng Engine, ref string) {
	t.Helper()
	target := t.TempDir()
	err := eng.Mount(context.Background(), ref, target)
	if skipMountEnvironment(err) {
		t.Skipf("mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "etc", "file.txt")) // #nosec G304 -- test mount path
	if err != nil || string(data) != modeBody {
		t.Errorf("read through the mount = %q, %v", data, err)
	}
	if err := eng.Unmount(context.Background(), target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
}
