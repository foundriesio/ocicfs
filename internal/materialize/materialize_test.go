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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
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
	dirBuilt, err := Materialize(context.Background(), dirObj, dirImg, dirSrc, lay.Root, lay.Platform, newSlots(2))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize dir source: %v", err)
	}

	memRoot, memObj, memImg := newStores(t)
	memBuilt, err := Materialize(context.Background(), memObj, memImg, readLayoutBlobs(t, lay.Dir), lay.Root, lay.Platform, newSlots(2))
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
	_, err = Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform, newSlots(2))
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
	_, err = Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform, newSlots(2))
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
			_, err = Materialize(context.Background(), obj, img, src, root, lay.Platform, newSlots(2))
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
		_, err = Materialize(context.Background(), obj, img, src, root, lay.Platform, newSlots(2))
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
		_, err = Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform, newSlots(2))
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
	src    ocichain.BlobSource
	layers []digest.Digest

	mu     sync.Mutex
	opened []int
}

func (l *openLog) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	l.mu.Lock()
	for i, d := range l.layers {
		if d == desc.Digest {
			l.opened = append(l.opened, i)
		}
	}
	l.mu.Unlock()
	return l.src.Open(ctx, desc)
}

// Opened returns a copy of the layer indexes opened so far, in order.
func (l *openLog) Opened() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.opened...)
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
	want, err := Materialize(context.Background(), wholeObj, wholeImg, src, lay.Root, lay.Platform, newSlots(1))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		layerRoot, layerObj, layerImg := newStores(t)
		b, err := Begin(context.Background(), layerObj, layerImg, src, lay.Root, lay.Platform, slots)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
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
	})
}

// TestBuildIngestForwardJumpThenNoop asserts Ingest(2) on a fresh build reads
// and merges layers 0, 1 and 2 in order, a following Ingest(1) reads nothing,
// and an index past the last layer is refused.
func TestBuildIngestForwardJumpThenNoop(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		src := &openLog{src: readLayoutBlobs(t, lay.Dir), layers: lay.Layers}
		_, obj, img := newStores(t)
		b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
		if err := b.Ingest(context.Background(), 2); err != nil {
			t.Fatalf("Ingest(2): %v", err)
		}
		if want := []int{0, 1, 2}; !reflect.DeepEqual(src.Opened(), want) {
			t.Fatalf("layers opened by Ingest(2) = %v, want %v", src.Opened(), want)
		}
		// An index at or below the merged prefix returns at once. There is no
		// assertion about opens here: the launcher opens ahead on its own
		// schedule, so what Ingest did or did not open is not its contract.
		if err := b.Ingest(context.Background(), 1); err != nil {
			t.Fatalf("Ingest(1) after Ingest(2): %v", err)
		}
		if err := b.Ingest(context.Background(), 3); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("Ingest(3) on a three-layer image = %v, want out-of-range error", err)
		}
	})
}

// TestBuildCloseLeavesNoTempsNoBinding asserts abandoning a build after two of
// three layers leaves no staging temp and writes no binding: the objects
// already sealed stay as unreferenced content.
func TestBuildCloseLeavesNoTempsNoBinding(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	root, obj, img := newStores(t)
	b, err := Begin(context.Background(), obj, img, readLayoutBlobs(t, lay.Dir), lay.Root, lay.Platform, newSlots(2))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := b.Ingest(context.Background(), 1); err != nil {
		t.Fatalf("Ingest(1): %v", err)
	}
	_ = b.Close()

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

// newSlots returns a scan limiter for n concurrent scans.
func newSlots(n int) chan struct{} {
	return make(chan struct{}, n)
}

// forEachCap runs fn as a subtest with a fresh limiter of capacity 1, 2 and 4.
func forEachCap(t *testing.T, fn func(t *testing.T, slots chan struct{})) {
	t.Helper()
	for _, n := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", n), func(t *testing.T) {
			fn(t, newSlots(n))
		})
	}
}

// layeredImage builds an n-layer image whose every layer stores a regular
// file over the inline threshold, so each layer's scan writes an object.
func layeredImage(t *testing.T, n int) octest.Layout {
	t.Helper()
	layers := make([]octest.LayerSpec, n)
	for i := range layers {
		body := fmt.Sprintf("layer %d payload larger than the sixty-four byte inline limit so it becomes an object\n", i)
		dir := fmt.Sprintf("d%d", i)
		layers[i] = octest.LayerSpec{Files: []octest.File{{Name: dir, Dir: true}, {Name: dir + "/f", Mode: 0o644, Body: body}}}
	}
	lay, err := octest.BuildImage(t.TempDir(), octest.ImageSpec{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Layers:   layers,
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	return lay
}

// gatedSource serves blobs from src, parking the gate layer's scan on its
// first Read until release is closed or the Open context is cancelled;
// entered is closed when it parks. It counts the readers it handed out and
// the ones closed again, so a test can tell no scan is still holding one.
type gatedSource struct {
	src     ocichain.BlobSource
	gate    digest.Digest
	entered chan struct{}
	release chan struct{}
	enter   sync.Once
	opened  atomic.Int32
	closed  atomic.Int32
}

func newGatedSource(src ocichain.BlobSource, gate digest.Digest) *gatedSource {
	return &gatedSource{src: src, gate: gate, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := g.src.Open(ctx, desc)
	if err != nil {
		return nil, err
	}
	g.opened.Add(1)
	r := &gatedReader{g: g, rc: rc}
	if desc.Digest == g.gate {
		r.ctx = ctx
		r.park = true
	}
	return r, nil
}

type gatedReader struct {
	g     *gatedSource
	rc    io.ReadCloser
	ctx   context.Context
	park  bool
	reads int
}

// Read parks the gated layer on the read after the one that opening it makes:
// OpenLayer sniffs the stream's compression before returning, so parking on
// the first read would hold the build's launcher inside Open, where it opens
// nothing further, rather than inside the layer's scan.
func (r *gatedReader) Read(p []byte) (int, error) {
	r.reads++
	if r.park && r.reads == 2 {
		r.park = false
		r.g.enter.Do(func() { close(r.g.entered) })
		select {
		case <-r.g.release:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	return r.rc.Read(p)
}

func (r *gatedReader) Close() error {
	r.g.closed.Add(1)
	return r.rc.Close()
}

// lateSource is a memory source whose blobs can be removed and added back
// while a build reads it.
type lateSource struct {
	mu    sync.Mutex
	blobs memorySource
}

func (s *lateSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blobs.Open(ctx, desc)
}

func (s *lateSource) take(d digest.Digest) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.blobs[d]
	delete(s.blobs, d)
	return data
}

func (s *lateSource) put(d digest.Digest, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[d] = data
}

// corruptBlob flips the last byte of the blob src holds under d, keeping its
// key so only content verification can catch the change.
func corruptBlob(src memorySource, d digest.Digest) {
	blob := append([]byte(nil), src[d]...)
	blob[len(blob)-1] ^= 0xff
	src[d] = blob
}

// waitFailed blocks until b has recorded a failure.
func waitFailed(t *testing.T, b *Build) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		err := b.err
		b.mu.Unlock()
		if err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("build recorded no failure")
	return nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// beginGated starts a build over a gated source for lay on fresh stores.
func beginGated(t *testing.T, lay octest.Layout, blobs memorySource, gate int, slots chan struct{}) (*Build, *gatedSource, string, *objectstore.Store, *imagestore.Store) {
	t.Helper()
	src := newGatedSource(blobs, lay.Layers[gate])
	root, obj, img := newStores(t)
	b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return b, src, root, obj, img
}

// TestBuildIngestBlocksUntilMerged asserts Ingest(0) does not return while
// layer 0 is still being read, and returns once it is merged.
func TestBuildIngestBlocksUntilMerged(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	blobs := readLayoutBlobs(t, lay.Dir)
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		b, src, _, _, _ := beginGated(t, lay, blobs, 0, slots)
		defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
		done := make(chan error, 1)
		go func() { done <- b.Ingest(context.Background(), 0) }()
		<-src.entered
		select {
		case err := <-done:
			t.Fatalf("Ingest(0) returned %v while layer 0 was parked", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(src.release)
		if err := <-done; err != nil {
			t.Fatalf("Ingest(0): %v", err)
		}
		b.mu.Lock()
		merged := b.mergedN
		b.mu.Unlock()
		if merged < 1 {
			t.Fatalf("Ingest(0) returned with %d layers merged", merged)
		}
		if _, err := b.Finish(context.Background()); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	})
}

// TestFinishCanceledDrains asserts a Finish whose context is cancelled while
// a layer is being read stops every scan before it returns: every opened
// layer is closed, no staging temp is left, and the store builds the image
// right after.
func TestFinishCanceledDrains(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	blobs := readLayoutBlobs(t, lay.Dir)
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		b, src, root, obj, img := beginGated(t, lay, blobs, 1, slots)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := b.Finish(ctx)
			done <- err
		}()
		<-src.entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Finish after cancel = %v, want context.Canceled", err)
		}
		if opened, closed := src.opened.Load(), src.closed.Load(); opened != closed {
			t.Fatalf("Finish returned with %d of %d opened layers still open", opened-closed, opened)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("Close after a cancelled Finish = %v, want nil", err)
		}
		temps, err := os.ReadDir(obj.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(temps) != 0 {
			t.Fatalf("temps left after a cancelled Finish: %d", len(temps))
		}
		if _, statErr := os.Stat(octest.BindingPath(root, lay.Root, lay.Platform)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("binding must not exist after a cancelled Finish, stat: %v", statErr)
		}
		if _, err := Materialize(context.Background(), obj, img, blobs, lay.Root, lay.Platform, slots); err != nil {
			t.Fatalf("Materialize after a cancelled Finish: %v", err)
		}
	})
}

// TestBuildScanErrorSticky asserts a layer that fails verification is the
// error every call reports from then on — Ingest, Finish and Close — and that
// the cancellation the failure induces in the other scans never surfaces as
// context.Canceled. Layer 0 is parked so, with more than one worker, layer 1
// fails first.
func TestBuildScanErrorSticky(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		blobs := readLayoutBlobs(t, lay.Dir)
		corruptBlob(blobs, lay.Layers[1])
		b, src, _, _, _ := beginGated(t, lay, blobs, 0, slots)
		done := make(chan error, 1)
		go func() { done <- b.Ingest(context.Background(), 0) }()
		// With one worker layer 1 cannot fail until layer 0 is let through;
		// with more, the failure cancels layer 0, which may not have reached
		// its gate yet, so wait for the failure rather than the gate.
		if cap(slots) == 1 {
			<-src.entered
		} else {
			_ = waitFailed(t, b)
		}
		close(src.release)
		if err := <-done; isContextError(err) {
			t.Fatalf("Ingest(0) = %v, want nil or the layer 1 failure", err)
		}
		first := b.Ingest(context.Background(), 1)
		if first == nil || isContextError(first) {
			t.Fatalf("Ingest(1) = %v, want the layer 1 failure", first)
		}
		if err := b.Ingest(context.Background(), 2); !errors.Is(err, first) {
			t.Fatalf("Ingest(2) = %v, want %v", err, first)
		}
		if _, err := b.Finish(context.Background()); !errors.Is(err, first) {
			t.Fatalf("Finish = %v, want %v", err, first)
		}
		if err := b.Close(); !errors.Is(err, first) {
			t.Fatalf("Close = %v, want %v", err, first)
		}
		if err := b.Close(); !errors.Is(err, first) {
			t.Fatalf("second Close = %v, want %v", err, first)
		}
	})
}

// TestBuildCloseInterruptsIngest asserts Close from another goroutine wakes a
// blocked Ingest, which reports the layer failure the build recorded, or
// ErrClosed when there is none; Close returns the same and is
// idempotent.
func TestBuildCloseInterruptsIngest(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		t.Run("no failure", func(t *testing.T) {
			b, src, _, _, _ := beginGated(t, lay, readLayoutBlobs(t, lay.Dir), 0, slots)
			done := make(chan error, 1)
			go func() { done <- b.Ingest(context.Background(), 0) }()
			<-src.entered
			if err := b.Close(); err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
			if err := <-done; !errors.Is(err, ErrClosed) {
				t.Fatalf("interrupted Ingest = %v, want ErrClosed", err)
			}
			if err := b.Close(); err != nil {
				t.Fatalf("second Close = %v, want nil", err)
			}
			if len(slots) != 0 {
				t.Fatalf("%d slots still held after Close", len(slots))
			}
		})
		if cap(slots) == 1 {
			return
		}
		t.Run("failure published", func(t *testing.T) {
			blobs := readLayoutBlobs(t, lay.Dir)
			corruptBlob(blobs, lay.Layers[1])
			b, _, _, _, _ := beginGated(t, lay, blobs, 0, slots)
			done := make(chan error, 1)
			go func() { done <- b.Ingest(context.Background(), 0) }()
			first := waitFailed(t, b)
			if err := b.Close(); !errors.Is(err, first) {
				t.Fatalf("Close = %v, want %v", err, first)
			}
			if err := <-done; !errors.Is(err, first) {
				t.Fatalf("interrupted Ingest = %v, want %v", err, first)
			}
			if err := b.Close(); !errors.Is(err, first) {
				t.Fatalf("second Close = %v, want %v", err, first)
			}
		})
	})
}

// TestBuildCloseBeforeFinishTail asserts a Finish whose layers were all
// merged before Close ran returns ErrClosed and writes no binding.
func TestBuildCloseBeforeFinishTail(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	root, obj, img := newStores(t)
	b, err := Begin(context.Background(), obj, img, readLayoutBlobs(t, lay.Dir), lay.Root, lay.Platform, newSlots(2))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := b.Ingest(context.Background(), 2); err != nil {
		t.Fatalf("Ingest(2): %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if _, err := b.Finish(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Finish after Close = %v, want ErrClosed", err)
	}
	if _, statErr := os.Stat(octest.BindingPath(root, lay.Root, lay.Platform)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("binding must not exist after Close, stat: %v", statErr)
	}
}

// TestBuildCloseWaitsForFinishTail races Close against a Finish whose layers
// are all merged, so Finish is writing the image and its binding, and
// asserts Close returns on one side of that tail or the other: either Finish
// already recorded the binding when Close returned, or Finish ends with
// ErrClosed and no binding.
func TestBuildCloseWaitsForFinishTail(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		root, obj, img := newStores(t)
		b, err := Begin(context.Background(), obj, img, readLayoutBlobs(t, lay.Dir), lay.Root, lay.Platform, slots)
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := b.Ingest(context.Background(), 2); err != nil {
			t.Fatalf("Ingest(2): %v", err)
		}
		type outcome struct {
			built Built
			err   error
		}
		done := make(chan outcome, 1)
		go func() {
			built, err := b.Finish(context.Background())
			done <- outcome{built, err}
		}()
		runtime.Gosched()
		if err := b.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		_, statErr := os.Stat(octest.BindingPath(root, lay.Root, lay.Platform))
		bound := statErr == nil
		out := <-done
		switch {
		case out.err == nil && !bound:
			t.Fatal("Finish succeeded but Close returned before the binding was written")
		case errors.Is(out.err, ErrClosed) && bound:
			t.Fatal("Finish returned ErrClosed but the binding was written")
		case out.err != nil && !errors.Is(out.err, ErrClosed):
			t.Fatalf("Finish = %v, want success or ErrClosed", out.err)
		}
	})
}

// TestBuildWindowBound asserts a build opens no layer more than twice the
// worker count ahead of the merged prefix: with layer 1 parked, layers up to
// 2×workers are opened and the next one is not until layer 1 merges.
func TestBuildWindowBound(t *testing.T) {
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		window := 2 * cap(slots)
		lay := layeredImage(t, window+2)
		digests := lay.Layers
		gated := newGatedSource(readLayoutBlobs(t, lay.Dir), digests[1])
		src := &openLog{src: gated, layers: digests}
		_, obj, img := newStores(t)
		b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
		if err := b.Ingest(context.Background(), 0); err != nil {
			t.Fatalf("Ingest(0): %v", err)
		}
		// The launcher opens a layer only once it holds a worker slot, so the
		// parked layer's slot is what stops it at one worker; with more, a
		// free slot cycles through the layers after it until the window does.
		ahead := window + 1
		if cap(slots) == 1 {
			ahead = 2
		}
		want := make([]int, ahead)
		for i := range want {
			want[i] = i
		}
		deadline := time.Now().Add(10 * time.Second)
		for len(src.Opened()) < len(want) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		if got := src.Opened(); !reflect.DeepEqual(got, want) {
			t.Fatalf("layers opened with layer 1 unmerged = %v, want %v", got, want)
		}
		close(gated.release)
		if _, err := b.Finish(context.Background()); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		want = make([]int, len(lay.Layers))
		for i := range want {
			want[i] = i
		}
		if got := src.Opened(); !reflect.DeepEqual(got, want) {
			t.Fatalf("layers opened by Finish = %v, want %v", got, want)
		}
	})
}

// TestBuildAbandonedReleasesSlots asserts a build closed with a scan in
// flight gives its slots back, so a build on the same limiter completes.
func TestBuildAbandonedReleasesSlots(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	blobs := readLayoutBlobs(t, lay.Dir)
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		b, src, _, _, _ := beginGated(t, lay, blobs, 1, slots)
		if err := b.Ingest(context.Background(), 0); err != nil {
			t.Fatalf("Ingest(0): %v", err)
		}
		<-src.entered
		if err := b.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		if len(slots) != 0 {
			t.Fatalf("%d slots still held after Close", len(slots))
		}
		_, obj, img := newStores(t)
		if _, err := Materialize(context.Background(), obj, img, blobs, lay.Root, lay.Platform, slots); err != nil {
			t.Fatalf("Materialize after an abandoned build: %v", err)
		}
	})
}

// TestBuildRetryYieldsSlot asserts a build polling for a layer its source
// does not hold yet leaves the shared worker slot alone: a second build on the
// same limiter runs to completion while the first is still retrying. Holding
// the slot across the pause would let one stalled pull stop every other build
// on the engine.
func TestBuildRetryYieldsSlot(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	slots := make(chan struct{}, 1)
	src := &lateSource{blobs: readLayoutBlobs(t, lay.Dir)}
	held := src.take(lay.Layers[1])

	_, obj, img := newStores(t)
	b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
	if err := b.Ingest(context.Background(), 0); err != nil {
		t.Fatalf("Ingest(0): %v", err)
	}

	// The launcher is polling for layer 1, which the caller has not asked for.
	_, obj2, img2 := newStores(t)
	blobs2 := readLayoutBlobs(t, lay.Dir)
	done := make(chan error, 1)
	go func() {
		_, err := Materialize(context.Background(), obj2, img2, blobs2, lay.Root, lay.Platform, slots)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second build while the first retried: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a second build got no slot while the first retried an absent layer")
	}

	src.put(lay.Layers[1], held)
	if _, err := b.Finish(context.Background()); err != nil {
		t.Fatalf("Finish once the layer appeared: %v", err)
	}
}

// TestBuildAbsentLayer asserts a layer blob the source does not hold yet is
// retried while the caller has not asked for it — the build completes once
// it appears — and fails the build once the caller has.
func TestBuildAbsentLayer(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	digests := lay.Layers
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		t.Run("above accepted retries", func(t *testing.T) {
			src := &lateSource{blobs: readLayoutBlobs(t, lay.Dir)}
			late := src.take(digests[2])
			_, obj, img := newStores(t)
			b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
			skipIfNoVerity(t, err)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
			for i := 0; i < 2; i++ {
				if err := b.Ingest(context.Background(), i); err != nil {
					t.Fatalf("Ingest(%d) with layer 2 absent: %v", i, err)
				}
			}
			time.Sleep(2 * retryAbsentLayer)
			src.put(digests[2], late)
			if _, err := b.Finish(context.Background()); err != nil {
				t.Fatalf("Finish after layer 2 appeared: %v", err)
			}
		})
		t.Run("at accepted fails", func(t *testing.T) {
			src := &lateSource{blobs: readLayoutBlobs(t, lay.Dir)}
			src.take(digests[1])
			_, obj, img := newStores(t)
			b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
			skipIfNoVerity(t, err)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
			first := b.Ingest(context.Background(), 1)
			if !errors.Is(first, fs.ErrNotExist) {
				t.Fatalf("Ingest(1) with layer 1 absent = %v, want fs.ErrNotExist", first)
			}
			if _, err := b.Finish(context.Background()); !errors.Is(err, first) {
				t.Fatalf("Finish = %v, want %v", err, first)
			}
		})
	})
}

// TestBuildIngestCancelled asserts an Ingest whose context is cancelled
// returns that error and leaves the build running: one cancelled before it
// starts asks for no layer, so a layer the source does not hold yet is still
// retried rather than failed; one cancelled during its wait is interrupted
// while the scan it waited on goes on. Either way the repeated call and
// Finish succeed.
func TestBuildIngestCancelled(t *testing.T) {
	lay := octest.ThreeLayerImage(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		t.Run("before the call", func(t *testing.T) {
			src := &lateSource{blobs: readLayoutBlobs(t, lay.Dir)}
			late := src.take(lay.Layers[0])
			_, obj, img := newStores(t)
			b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, slots)
			skipIfNoVerity(t, err)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := b.Ingest(ctx, 0); !errors.Is(err, context.Canceled) {
				t.Fatalf("Ingest(0) under a cancelled ctx = %v, want context.Canceled", err)
			}
			b.mu.Lock()
			accepted, launched := b.accepted, b.launched
			b.mu.Unlock()
			if accepted != -1 || launched {
				t.Fatalf("cancelled Ingest(0) asked for layer %d and launched=%v, want none and false", accepted, launched)
			}
			src.put(lay.Layers[0], late)
			if err := b.Ingest(context.Background(), 0); err != nil {
				t.Fatalf("Ingest(0) repeated: %v", err)
			}
			if _, err := b.Finish(context.Background()); err != nil {
				t.Fatalf("Finish: %v", err)
			}
		})
		t.Run("during the wait", func(t *testing.T) {
			blobs := readLayoutBlobs(t, lay.Dir)
			b, src, _, _, _ := beginGated(t, lay, blobs, 0, slots)
			defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- b.Ingest(ctx, 0) }()
			<-src.entered
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("Ingest(0) after cancel = %v, want context.Canceled", err)
			}
			close(src.release)
			if err := b.Ingest(context.Background(), 0); err != nil {
				t.Fatalf("Ingest(0) repeated: %v", err)
			}
			if _, err := b.Finish(context.Background()); err != nil {
				t.Fatalf("Finish: %v", err)
			}
		})
	})
}
