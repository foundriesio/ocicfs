// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/octest"
)

// TestPullReusesSavedLayerAfterGCE2E pulls one release, collects, and pulls
// the next release, which shares the first one's base layer, from a source
// that no longer holds that layer's blob. The collection reclaims the files
// of the base the first release shadows and keeps the base's saved layer; the
// second pull merges the base from it, never asks for its blob, and records
// the binding a pull into an empty store records. Removing the first release
// then lets its own layer's saved layer go and keeps the shared one.
func TestPullReusesSavedLayerAfterGCE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	freshRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})
	ctx := context.Background()

	body := func(label string) string {
		return label + ": a payload larger than the sixty-four byte inline limit so it becomes an object\n"
	}
	release := func(name string) octest.Layout {
		lay, err := octest.BuildImage(t.TempDir(), octest.ImageSpec{
			Platform: hostPlatform(),
			Layers: []octest.LayerSpec{
				{Files: []octest.File{
					{Name: "usr", Dir: true},
					{Name: "usr/bin", Dir: true},
					{Name: "usr/bin/app", Mode: 0o755, Body: body("app")},
					{Name: "usr/bin/edited", Mode: 0o755, Body: body("edited in the base")},
					{Name: "usr/bin/removed", Mode: 0o755, Body: body("removed")},
				}},
				{Files: []octest.File{
					{Name: "usr/bin/edited", Mode: 0o755, Body: body("edited in " + name)},
					{Name: "usr/bin/.wh.removed"},
				}},
			},
		})
		if err != nil {
			t.Fatalf("BuildImage: %v", err)
		}
		return lay
	}
	layA, layB := release("a"), release("b")
	base := layA.Layers[0]
	if layB.Layers[0] != base {
		t.Fatalf("the two releases do not share their base layer: %s and %s", base, layB.Layers[0])
	}

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(ctx, layA.Ref, WithFromDir(layA.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull(a): %v", err)
	}
	if _, err := eng.GC(ctx); err != nil {
		t.Fatalf("GC: %v", err)
	}
	assertObjectAbsent(t, storeRoot, digest.FromString(body("edited in the base")))
	assertObjectAbsent(t, storeRoot, digest.FromString(body("removed")))

	src := layoutBlobs(t, layB.Dir)
	delete(src.blobs, base)
	got, err := eng.Pull(ctx, layB.Ref, WithBlobSource(src))
	if err != nil {
		t.Fatalf("Pull(b) from a source without the shared layer: %v", err)
	}
	want := pullInto(t, freshRoot, layB.Ref, WithFromDir(layB.Dir))
	if got.Digest != want.Digest || got.Size != want.Size {
		t.Fatalf("image over the saved layer = %#v, image in an empty store = %#v", got, want)
	}
	gotBinding, err := os.ReadFile(octest.BindingPath(storeRoot, layB.Root, layB.Platform)) // #nosec G304 -- test reads a store it wrote
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	wantBinding, err := os.ReadFile(octest.BindingPath(freshRoot, layB.Root, layB.Platform)) // #nosec G304 -- test reads a store it wrote
	if err != nil {
		t.Fatalf("read binding of the empty-store pull: %v", err)
	}
	if !bytes.Equal(gotBinding, wantBinding) {
		t.Fatalf("binding over the saved layer:\n%s\nbinding in an empty store:\n%s", gotBinding, wantBinding)
	}

	target := t.TempDir()
	if err := eng.Mount(ctx, layB.Ref, target); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Unmount(context.Background(), target); err != nil && !errors.Is(err, ErrNotMounted) {
			t.Errorf("Unmount: %v", err)
		}
	})
	for name, wantBody := range map[string]string{"app": body("app"), "edited": body("edited in b")} {
		data, err := os.ReadFile(filepath.Join(target, "usr", "bin", name)) // #nosec G304 -- mount target under t-managed dir
		if err != nil || string(data) != wantBody {
			t.Fatalf("read usr/bin/%s through the mount = %q, %v", name, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "usr", "bin", "removed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed file through the mount: %v, want it absent", err)
	}
	if err := eng.Unmount(ctx, target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	if err := eng.Remove(ctx, layA.Ref); err != nil {
		t.Fatalf("Remove(a): %v", err)
	}
	if _, err := eng.GC(ctx); err != nil {
		t.Fatalf("GC after remove: %v", err)
	}
	img, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	if _, err := img.GetLayer(base); err != nil {
		t.Fatalf("the saved layer the remaining image lists: %v", err)
	}
	if _, err := img.GetLayer(layA.Layers[1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the saved layer of the removed image's own layer: %v, want it gone", err)
	}
}
