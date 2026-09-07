// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package materialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/octest"
)

// canonicalLayerFiles is a standard-layout single-layer tree — /usr and its
// intermediate directories are explicit, so the construction is canonical.
func canonicalLayerFiles(body string) []octest.File {
	return []octest.File{
		{Name: "usr", Dir: true},
		{Name: "usr/bin", Dir: true},
		{Name: "usr/bin/app", Mode: 0o755, Body: body},
	}
}

func TestMaterializeMatchesDirectoryAndMemorySources(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), ocispec.Platform{OS: "linux", Architecture: "amd64"},
		// Over the inline threshold, so it is stored as a CAS object (FileDigest).
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr - a config payload larger than the sixty-four byte inline limit\n"},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: "#!/bin/sh\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	dirRoot, dirObj, dirImg := newStores(t)
	dirSrc, err := ocilayout.Open(lay.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	dirBuilt, err := Materialize(context.Background(), dirObj, dirImg, dirSrc, lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize dir source: %v", err)
	}

	memRoot, memObj, memImg := newStores(t)
	memBuilt, err := Materialize(context.Background(), memObj, memImg, readLayoutBlobs(t, lay.Dir), lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize memory source: %v", err)
	}

	if !sameBuilt(dirBuilt, memBuilt) {
		t.Fatalf("built mismatch:\n dir=%#v\n mem=%#v", dirBuilt, memBuilt)
	}
	assertObjectBytesEqual(t, dirRoot, memRoot, lay.Root)
	assertObjectBytesEqual(t, dirRoot, memRoot, lay.Manifest)
	assertObjectBytesEqual(t, dirRoot, memRoot, lay.Config)
	assertObjectBytesEqual(t, dirRoot, memRoot, lay.FileDigest)
	assertObjectBytesEqual(t, dirRoot, memRoot, dirBuilt.Descriptor.Digest)
	assertFileBytesEqual(t, octest.BindingPath(dirRoot, lay.Root, lay.Platform), octest.BindingPath(memRoot, lay.Root, lay.Platform))
}

// TestMaterializePersistsCanonicalFlag asserts the binding written by Materialize
// records the construction's canonical flag: true for a standard-layout image
// (explicit parents, /usr a directory) and false for a divergent one (implicit
// parents, no /usr).
func TestMaterializePersistsCanonicalFlag(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	// Over the inline threshold so the file is a real CAS object.
	body := "a config payload larger than the sixty-four byte inline limit so it becomes an object\n"
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}

	canonical, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "usr/bin", Dir: true},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: body},
	)
	if err != nil {
		t.Fatalf("build canonical layout: %v", err)
	}
	divergent, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "etc/config", Mode: 0o644, Body: body},
	)
	if err != nil {
		t.Fatalf("build divergent layout: %v", err)
	}

	if got := materializeCanonical(t, canonical); !got {
		t.Fatal("standard-layout binding recorded canonical = false")
	}
	if got := materializeCanonical(t, divergent); got {
		t.Fatal("divergent binding recorded canonical = true")
	}
}

// materializeCanonical materializes lay and returns the canonical flag recorded
// in its binding.
func materializeCanonical(t *testing.T, lay octest.Layout) bool {
	t.Helper()
	_, obj, img := newStores(t)
	src, err := ocilayout.Open(lay.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	_, err = Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	b, err := img.Get(lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("imagestore.Get: %v", err)
	}
	return b.Canonical
}

type memorySource map[digest.Digest][]byte

func (s memorySource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := s[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("materialize: memory blob %s: %w", desc.Digest, os.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func newStores(t *testing.T) (string, *objectstore.Store, *imagestore.Store) {
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
	img, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	return root, obj, img
}

func skipIfNoVerity(t *testing.T, err error) {
	t.Helper()
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
}

func sameBuilt(a, b Built) bool {
	return a.Root == b.Root &&
		samePlatform(a.Platform, b.Platform) &&
		sameDescriptor(a.Descriptor, b.Descriptor) &&
		a.FsverityDigest == b.FsverityDigest &&
		sameDescriptor(a.RootDesc, b.RootDesc)
}

func samePlatform(a, b ocispec.Platform) bool {
	return a.OS == b.OS &&
		a.Architecture == b.Architecture &&
		a.Variant == b.Variant
}

func sameDescriptor(a, b ocispec.Descriptor) bool {
	return a.MediaType == b.MediaType &&
		a.Digest == b.Digest &&
		a.Size == b.Size
}

func readLayoutBlobs(t *testing.T, dir string) memorySource {
	t.Helper()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	out := make(memorySource, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(blobDir, entry.Name())) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		d := digest.Digest("sha256:" + entry.Name())
		if err := d.Validate(); err != nil {
			t.Fatalf("invalid fixture digest %s: %v", d, err)
		}
		out[d] = data
	}
	return out
}

func assertObjectBytesEqual(t *testing.T, leftRoot, rightRoot string, d digest.Digest) {
	t.Helper()
	assertFileBytesEqual(t, octest.ObjectPath(leftRoot, d), octest.ObjectPath(rightRoot, d))
}

func assertFileBytesEqual(t *testing.T, leftPath, rightPath string) {
	t.Helper()
	left, err := os.ReadFile(leftPath) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", leftPath, err)
	}
	right, err := os.ReadFile(rightPath) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", rightPath, err)
	}
	if !bytes.Equal(left, right) {
		t.Fatalf("%s and %s differ", leftPath, rightPath)
	}
}

// TestMaterializeCorruptLayerWritesNoBinding pins the write ordering a failed
// pull relies on: when a layer fails verification, Materialize errors out and
// no image binding is written, so nothing references whatever objects were
// already stored.
func TestMaterializeCorruptLayerWritesNoBinding(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), ocispec.Platform{OS: "linux", Architecture: "amd64"},
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	corruptLayerBlob(t, lay)

	root, obj, img := newStores(t)
	src, err := ocilayout.Open(lay.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	_, err = Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err == nil {
		t.Fatal("expected Materialize to fail on a corrupt layer")
	}
	if _, statErr := os.Stat(octest.BindingPath(root, lay.Root, lay.Platform)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("binding must not exist after a failed pull, stat: %v", statErr)
	}
}

// TestMaterializeRejectsTamperedSealingAnnotations pins the fresh-path
// fail-closed behavior for a canonical image: a merged or config sealing
// annotation that disagrees with the measured content aborts the pull before
// any binding is written.
func TestMaterializeRejectsTamperedSealingAnnotations(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	tampered := strings.Repeat("0", 64)
	tests := []struct {
		name        string
		manifestAnn map[string]string
		configAnn   map[string]string
	}{
		{name: "merged", manifestAnn: map[string]string{ociseal.MergedAnnotation: tampered}},
		{name: "config", configAnn: map[string]string{ociseal.ConfigAnnotation: tampered}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lay, err := octest.BuildSingleLayerGzip(t.TempDir(), ocispec.Platform{OS: "linux", Architecture: "amd64"},
				canonicalLayerFiles("hello from knarr - a payload past the inline limit\n")...,
			)
			if err != nil {
				t.Fatalf("build layout: %v", err)
			}
			root := rewriteManifestAnnotations(t, lay, tt.manifestAnn, tt.configAnn)
			storeRoot, obj, img := newStores(t)
			src, err := ocilayout.Open(lay.Dir)
			if err != nil {
				t.Fatalf("ocilayout.Open: %v", err)
			}
			_, err = Materialize(context.Background(), obj, img, src, root, lay.Platform)
			skipIfNoVerity(t, err)
			if err == nil || !strings.Contains(err.Error(), "annotation") {
				t.Fatalf("Materialize error = %v, want annotation failure", err)
			}
			if _, statErr := os.Stat(octest.BindingPath(storeRoot, root, lay.Platform)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("binding must not exist after annotation failure, stat: %v", statErr)
			}
		})
	}
}

// TestMaterializeNonCanonicalSealingPolicy pins the fresh-path rule for a
// non-canonical image: a merged-EROFS annotation under any suffix key fails
// closed, while the same image with no such annotation is a valid unsealed
// pull.
func TestMaterializeNonCanonicalSealingPolicy(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	body := "a payload larger than the sixty-four byte inline limit so it becomes an object\n"
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}

	t.Run("sealed rejected", func(t *testing.T) {
		lay, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
			octest.File{Name: "etc/config", Mode: 0o644, Body: body},
		)
		if err != nil {
			t.Fatalf("build layout: %v", err)
		}
		root := rewriteManifestAnnotations(t, lay,
			map[string]string{"composefs.merged.erofs.v1.fsverity-sha512-12": strings.Repeat("a", 64)}, nil)
		storeRoot, obj, img := newStores(t)
		src, err := ocilayout.Open(lay.Dir)
		if err != nil {
			t.Fatalf("ocilayout.Open: %v", err)
		}
		_, err = Materialize(context.Background(), obj, img, src, root, lay.Platform)
		skipIfNoVerity(t, err)
		if err == nil || !strings.Contains(err.Error(), "non-canonical") {
			t.Fatalf("Materialize error = %v, want non-canonical failure", err)
		}
		if _, statErr := os.Stat(octest.BindingPath(storeRoot, root, lay.Platform)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("binding must not exist after annotation failure, stat: %v", statErr)
		}
	})

	t.Run("unsealed accepted", func(t *testing.T) {
		lay, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
			octest.File{Name: "etc/config", Mode: 0o644, Body: body},
		)
		if err != nil {
			t.Fatalf("build layout: %v", err)
		}
		_, obj, img := newStores(t)
		src, err := ocilayout.Open(lay.Dir)
		if err != nil {
			t.Fatalf("ocilayout.Open: %v", err)
		}
		_, err = Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform)
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("Materialize: %v", err)
		}
		b, err := img.Get(lay.Root, lay.Platform)
		if err != nil {
			t.Fatalf("imagestore.Get: %v", err)
		}
		if b.Canonical {
			t.Fatal("divergent binding recorded canonical = true")
		}
	})
}

// openLog records which layer blobs a source was asked to open, so a test can
// tell which layers an Ingest actually read.
type openLog struct {
	src    memorySource
	layers []digest.Digest
	opened []int
}

func (l *openLog) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	for i, d := range l.layers {
		if d == desc.Digest {
			l.opened = append(l.opened, i)
		}
	}
	return l.src.Open(ctx, desc)
}

// TestBuildMatchesMaterialize asserts Begin, one Ingest per layer, and Finish
// produce the same binding, image object, and result as Materialize.
func TestBuildMatchesMaterialize(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	src := readLayoutBlobs(t, lay.Dir)

	wholeRoot, wholeObj, wholeImg := newStores(t)
	want, err := Materialize(context.Background(), wholeObj, wholeImg, src, lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	layerRoot, layerObj, layerImg := newStores(t)
	b, err := Begin(context.Background(), layerObj, layerImg, src, lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer b.Close()
	for i := range lay.DiffIDs {
		if err := b.Ingest(context.Background(), i); err != nil {
			t.Fatalf("Ingest(%d): %v", i, err)
		}
	}
	got, err := b.Finish(context.Background())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !sameBuilt(got, want) {
		t.Fatalf("built mismatch:\n layer=%#v\n whole=%#v", got, want)
	}
	assertObjectBytesEqual(t, wholeRoot, layerRoot, want.Descriptor.Digest)
	assertFileBytesEqual(t, octest.BindingPath(wholeRoot, lay.Root, lay.Platform), octest.BindingPath(layerRoot, lay.Root, lay.Platform))
}

// TestBuildIngestForwardJumpThenNoop asserts Ingest(2) on a fresh build reads
// and applies layers 0, 1 and 2 in order, a following Ingest(1) reads nothing,
// and an index past the last layer is refused.
func TestBuildIngestForwardJumpThenNoop(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	src := &openLog{src: readLayoutBlobs(t, lay.Dir), layers: lay.Layers}

	_, obj, img := newStores(t)
	b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer b.Close()
	if err := b.Ingest(context.Background(), 2); err != nil {
		t.Fatalf("Ingest(2): %v", err)
	}
	if want := []int{0, 1, 2}; !reflect.DeepEqual(src.opened, want) {
		t.Fatalf("layers opened by Ingest(2) = %v, want %v", src.opened, want)
	}
	if err := b.Ingest(context.Background(), 1); err != nil {
		t.Fatalf("Ingest(1) after Ingest(2): %v", err)
	}
	if want := []int{0, 1, 2}; !reflect.DeepEqual(src.opened, want) {
		t.Fatalf("layers opened after Ingest(1) = %v, want no new opens over %v", src.opened, want)
	}
	if err := b.Ingest(context.Background(), 3); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("Ingest(3) on a three-layer image = %v, want out-of-range error", err)
	}
}

// TestBuildCloseLeavesNoTempsNoBinding asserts abandoning a build after two of
// three layers leaves no staging temp and writes no binding: the objects
// already sealed stay as unreferenced content.
func TestBuildCloseLeavesNoTempsNoBinding(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	root, obj, img := newStores(t)
	b, err := Begin(context.Background(), obj, img, readLayoutBlobs(t, lay.Dir), lay.Root, lay.Platform)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := b.Ingest(context.Background(), 1); err != nil {
		t.Fatalf("Ingest(1): %v", err)
	}
	b.Close()

	temps, err := os.ReadDir(obj.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temps left after Close: %d", len(temps))
	}
	if _, statErr := os.Stat(octest.BindingPath(root, lay.Root, lay.Platform)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("binding must not exist after Close, stat: %v", statErr)
	}
	if err := b.Ingest(context.Background(), 2); err == nil {
		t.Fatal("Ingest after Close succeeded")
	}
}

// corruptLayerBlob flips the last byte of the layout's single layer blob in
// place, keeping its length so only content verification can catch the change.
func corruptLayerBlob(t *testing.T, lay octest.Layout) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lay.Dir, "blobs", "sha256", lay.Manifest.Encoded())) // #nosec G304 G703 -- test reads the layout it built
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	layerPath := filepath.Join(lay.Dir, "blobs", "sha256", m.Layers[0].Digest.Encoded())
	blob, err := os.ReadFile(layerPath) // #nosec G304 G703 -- test reads the layout it built
	if err != nil {
		t.Fatalf("read layer: %v", err)
	}
	blob[len(blob)-1] ^= 0xff
	if err := os.WriteFile(layerPath, blob, 0o644); err != nil { // #nosec G703 -- test corrupts a layer blob in the layout it built
		t.Fatalf("write corrupt layer: %v", err)
	}
}

// rewriteManifestAnnotations rewrites lay's manifest with the given manifest and
// config-descriptor annotations, writes it under its new digest, and returns
// that digest as the pinned root.
func rewriteManifestAnnotations(t *testing.T, lay octest.Layout, manifestAnn, configAnn map[string]string) digest.Digest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lay.Dir, "blobs", "sha256", lay.Manifest.Encoded())) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifestAnn != nil {
		manifest.Annotations = manifestAnn
	}
	if configAnn != nil {
		manifest.Config.Annotations = configAnn
	}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	root := digest.FromBytes(raw)
	if err := os.WriteFile(filepath.Join(lay.Dir, "blobs", "sha256", root.Encoded()), raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return root
}
