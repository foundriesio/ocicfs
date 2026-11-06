// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package materialize

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/ociseal"
	"github.com/foundriesio/ocicfs/internal/octest"
	"github.com/foundriesio/ocicfs/internal/rootfs"
	"github.com/foundriesio/ocicfs/internal/verity"
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
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from ocicfs - a config payload larger than the sixty-four byte inline limit\n"},
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

// TestMaterializeStoreModesMatchOnDisk asserts a store without fs-verity
// holds exactly the bytes a store with fs-verity holds for the same image:
// every name, file type, symlink target and file content agrees. The store
// without it is an auto build's store on the test temp dir, whose filesystem
// cannot enable fs-verity.
func TestMaterializeStoreModesMatchOnDisk(t *testing.T) {
	if verity.Mode != verity.ModeAuto {
		t.Skip("needs an auto build, which takes both modes from the filesystem")
	}
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), ocispec.Platform{OS: "linux", Architecture: "amd64"},
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from ocicfs - a config payload larger than the sixty-four byte inline limit\n"},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: strings.Repeat("binary ", 2000)},
		octest.File{Name: "etc/link", Link: "etc/config.txt"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	roots := map[verity.Policy]string{}
	for _, mode := range []verity.Policy{verity.ModeOff, verity.ModeRequired} {
		var root string
		var obj *objectstore.Store
		var img *imagestore.Store
		if mode == verity.ModeOff {
			root, obj, img = newPlainStores(t)
		} else {
			root, obj, img = newStores(t)
		}
		if got, err := obj.Mode(); err != nil || got != mode {
			t.Fatalf("Mode = %q, %v; want %q", got, err, mode)
		}
		src, err := ocilayout.Open(lay.Dir)
		if err != nil {
			t.Fatalf("ocilayout.Open: %v", err)
		}
		if _, err := Materialize(context.Background(), obj, img, src, lay.Root, lay.Platform, newSlots(2)); err != nil {
			t.Fatalf("Materialize into the %s store: %v", mode, err)
		}
		roots[mode] = root
	}
	off, required := treeContents(t, roots[verity.ModeOff]), treeContents(t, roots[verity.ModeRequired])
	if !reflect.DeepEqual(off, required) {
		for name, v := range required {
			if off[name] != v {
				t.Errorf("%s differs: off store %.40q, required store %.40q", name, off[name], v)
			}
		}
		for name := range off {
			if _, ok := required[name]; !ok {
				t.Errorf("%s is only in the off store", name)
			}
		}
		t.Fatal("the off and required stores differ on disk")
	}
}

// treeContents maps every path under root to its type and its content or
// link target.
func treeContents(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case de.IsDir():
			out[rel] = "dir"
		case de.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
		default:
			data, err := os.ReadFile(path) // #nosec G304 G122 -- test store path
			if err != nil {
				return err
			}
			out[rel] = "file " + string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
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

// newPlainStores opens the stores on the test temp dir, skipping when its
// filesystem can enable fs-verity.
func newPlainStores(t *testing.T) (string, *objectstore.Store, *imagestore.Store) {
	t.Helper()
	root := t.TempDir()
	obj, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	if obj.CheckVerity() == nil {
		t.Skip("temp dir filesystem unexpectedly supports fs-verity")
	}
	img, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	return root, obj, img
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
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from ocicfs\n"},
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
				canonicalLayerFiles("hello from ocicfs - a payload past the inline limit\n")...,
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

// stored returns a body over the inline threshold, so a file carrying it is
// kept as an object.
func stored(label string) string {
	return label + ": a payload larger than the sixty-four byte inline limit so it becomes an object\n"
}

// storedDigest is the digest of the object a file with the body stored(label)
// is kept as.
func storedDigest(label string) digest.Digest {
	return digest.FromString(stored(label))
}

// sharedBase is the layer two releases of an image share: a canonical tree
// with one file the upper layer of every release overwrites, one it may
// remove, a hardlink and an inlined file.
func sharedBase() []octest.File {
	return []octest.File{
		{Name: "usr", Dir: true},
		{Name: "usr/bin", Dir: true},
		{Name: "usr/bin/app", Mode: 0o755, Body: stored("app")},
		{Name: "usr/bin/edited", Mode: 0o755, Body: stored("edited in the base")},
		{Name: "usr/bin/removed", Mode: 0o755, Body: stored("removed")},
		{Name: "usr/bin/alias", Link: "usr/bin/app"},
		{Name: "usr/bin/tiny", Mode: 0o644, Body: "tiny"},
	}
}

// releaseLayer is the upper layer of one release: it overwrites a file of the
// base, adds one of its own, and removes another file of the base when
// whiteout is set.
func releaseLayer(release string, whiteout bool) []octest.File {
	files := []octest.File{
		{Name: "usr/bin/edited", Mode: 0o755, Body: stored("edited in " + release)},
		{Name: "usr/bin/" + release, Mode: 0o755, Body: stored(release)},
	}
	if whiteout {
		files = append(files, octest.File{Name: "usr/bin/.wh.removed"})
	}
	return files
}

// buildLayers builds an image of the given layers for the fixture platform.
func buildLayers(t *testing.T, layers ...[]octest.File) octest.Layout {
	t.Helper()
	specs := make([]octest.LayerSpec, len(layers))
	for i, files := range layers {
		specs[i] = octest.LayerSpec{Files: files}
	}
	lay, err := octest.BuildImage(t.TempDir(), octest.ImageSpec{Platform: fixturePlatform, Layers: specs})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	return lay
}

// pullLayout materializes the image root of lay from its layout directory.
func pullLayout(t *testing.T, obj *objectstore.Store, img *imagestore.Store, lay octest.Layout, root digest.Digest) Built {
	t.Helper()
	built, err := Materialize(context.Background(), obj, img, openLayout(t, lay.Dir), root, lay.Platform, newSlots(2))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return built
}

// assertMatchesFreshPull asserts got, built in the store at gotRoot, is the
// image a pull of the same root into an empty store builds.
func assertMatchesFreshPull(t *testing.T, lay octest.Layout, root digest.Digest, gotRoot string, got Built) {
	t.Helper()
	wantRoot, wantObj, wantImg := newStores(t)
	assertSameImage(t, wantRoot, pullLayout(t, wantObj, wantImg, lay, root), gotRoot, got)
}

// removeObjects removes the named objects from the store, as a collection
// that found them unreferenced does.
func removeObjects(t *testing.T, obj *objectstore.Store, digests ...digest.Digest) {
	t.Helper()
	for _, d := range digests {
		fsv, err := obj.VerityDigest(context.Background(), d)
		if err != nil {
			t.Fatalf("VerityDigest(%s): %v", d, err)
		}
		hex := strings.TrimPrefix(fsv, "sha256:")
		if err := obj.Remove(d, hex[:2]+"/"+hex[2:]); err != nil {
			t.Fatalf("Remove(%s): %v", d, err)
		}
	}
}

// assertSaved asserts the store holds, or does not hold, a saved layer for
// each blob.
func assertSaved(t *testing.T, img *imagestore.Store, want bool, blobs ...digest.Digest) {
	t.Helper()
	for _, blob := range blobs {
		_, err := img.GetLayer(blob)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("GetLayer(%s): %v", blob, err)
		}
		if got := err == nil; got != want {
			t.Fatalf("saved layer of %s present = %v, want %v", blob, got, want)
		}
	}
}

// TestSavedLayerReused asserts a pull saves every layer it scans, and that a
// later pull of an image sharing a layer merges it from the saved layer
// without opening its blob — Ingest of that layer included — and builds the
// image a pull into an empty store builds. A pull that finds no saved layer
// reuses none.
func TestSavedLayerReused(t *testing.T) {
	requireTools(t)
	layA := buildLayers(t, sharedBase(), releaseLayer("a", true))
	layB := buildLayers(t, sharedBase(), releaseLayer("b", true))
	base := layA.Layers[0]
	if layB.Layers[0] != base {
		t.Fatalf("the two releases do not share their base layer: %s and %s", base, layB.Layers[0])
	}
	forEachCap(t, func(t *testing.T, slots chan struct{}) {
		root, obj, img := newStores(t)
		first, err := Begin(context.Background(), obj, img, openLayout(t, layA.Dir), layA.Root, layA.Platform, slots)
		skipIfNoVerity(t, err)
		if err != nil {
			t.Fatalf("Begin(a): %v", err)
		}
		defer first.Close() //nolint:errcheck // a failure is reported by the call that hit it
		if _, err := first.Finish(context.Background()); err != nil {
			t.Fatalf("Finish(a): %v", err)
		}
		if len(first.reused) != 0 {
			t.Fatalf("a pull into an empty store reused layers %v", first.reused)
		}
		assertSaved(t, img, true, layA.Layers...)
		assertSaved(t, img, false, layB.Layers[1])

		rec := newRecorder(t, layB.Dir)
		rec.failOpen = map[digest.Digest]error{base: errInjected}
		b, err := Begin(context.Background(), obj, img, rec, layB.Root, layB.Platform, slots)
		if err != nil {
			t.Fatalf("Begin(b): %v", err)
		}
		defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
		for i := range layB.Layers {
			if err := b.Ingest(context.Background(), i); err != nil {
				t.Fatalf("Ingest(%d): %v", i, err)
			}
		}
		got, err := b.Finish(context.Background())
		if err != nil {
			t.Fatalf("Finish(b): %v", err)
		}
		rec.assertOpens(t, []digest.Digest{layB.Layers[1]})
		if !reflect.DeepEqual(b.reused, []int{0}) {
			t.Fatalf("reused layers = %v, want the base alone", b.reused)
		}
		assertMatchesFreshPull(t, layB, layB.Root, root, got)
		assertSaved(t, img, true, layB.Layers...)
		if len(slots) != 0 {
			t.Fatalf("%d slots still held after the pull", len(slots))
		}
	})
}

// TestSavedLayerShadowedFiles asserts a file of a saved layer that an upper
// layer overwrites or removes is never asked for: with their objects
// collected, the layer is still not opened. Once the new release stops
// removing such a file, the image needs it, and the layer is read whole,
// once.
func TestSavedLayerShadowedFiles(t *testing.T) {
	requireTools(t)
	layA := buildLayers(t, sharedBase(), releaseLayer("a", true))
	base := layA.Layers[0]
	for _, c := range []struct {
		name     string
		whiteout bool
		opens    func(lay octest.Layout) []digest.Digest
	}{
		{"still removed", true, func(lay octest.Layout) []digest.Digest { return []digest.Digest{lay.Layers[1]} }},
		{"no longer removed", false, func(lay octest.Layout) []digest.Digest { return []digest.Digest{base, lay.Layers[1]} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, obj, img := newStores(t)
			pullLayout(t, obj, img, layA, layA.Root)
			// What a collection reclaims after the first pull: the files of
			// the base its own upper layer shadows.
			removeObjects(t, obj, storedDigest("edited in the base"), storedDigest("removed"))

			layB := buildLayers(t, sharedBase(), releaseLayer("b", c.whiteout))
			rec := newRecorder(t, layB.Dir)
			got, err := Materialize(context.Background(), obj, img, rec, layB.Root, layB.Platform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize(b): %v", err)
			}
			rec.assertOpens(t, c.opens(layB))
			assertMatchesFreshPull(t, layB, layB.Root, root, got)
			present, err := obj.Has(storedDigest("removed"))
			if err != nil {
				t.Fatalf("Has: %v", err)
			}
			if present == c.whiteout {
				t.Fatalf("the removed file's object present = %v with whiteout = %v", present, c.whiteout)
			}
		})
	}
}

// TestSavedLayerMissingObjects asserts a saved layer whose files the store no
// longer holds — one of them, or all of them, as after a crash that kept the
// saved layer and lost the objects — is read whole, once, and the image built
// is the one built before.
func TestSavedLayerMissingObjects(t *testing.T) {
	requireTools(t)
	lay := buildLayers(t, sharedBase(), releaseLayer("a", true))
	for _, c := range []struct {
		name    string
		removed []digest.Digest
	}{
		{"one needed object", []digest.Digest{storedDigest("app")}},
		{"every object of the layer", []digest.Digest{storedDigest("app"), storedDigest("edited in the base"), storedDigest("removed")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, obj, img := newStores(t)
			want := pullLayout(t, obj, img, lay, lay.Root)
			removeObjects(t, obj, c.removed...)

			rec := newRecorder(t, lay.Dir)
			got, err := Materialize(context.Background(), obj, img, rec, lay.Root, lay.Platform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize again: %v", err)
			}
			rec.assertOpens(t, []digest.Digest{lay.Layers[0]})
			if !sameBuilt(got, want) {
				t.Fatalf("built mismatch:\n again=%#v\n first=%#v", got, want)
			}
			assertMatchesFreshPull(t, lay, lay.Root, root, got)
		})
	}
}

// TestSavedLayerRunFile asserts the documented over-fetch: a file under /run
// survives the layer merge and is dropped only afterwards, so a saved layer
// listing one the store lacks is read although the image does not keep the
// file.
func TestSavedLayerRunFile(t *testing.T) {
	requireTools(t)
	lay := buildLayers(t, append(sharedBase(),
		octest.File{Name: "run", Dir: true},
		octest.File{Name: "run/state", Mode: 0o644, Body: stored("run state")},
	), releaseLayer("a", true))
	root, obj, img := newStores(t)
	pullLayout(t, obj, img, lay, lay.Root)
	removeObjects(t, obj, storedDigest("run state"))

	rec := newRecorder(t, lay.Dir)
	got, err := Materialize(context.Background(), obj, img, rec, lay.Root, lay.Platform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize again: %v", err)
	}
	rec.assertOpens(t, []digest.Digest{lay.Layers[0]})
	assertMatchesFreshPull(t, lay, lay.Root, root, got)
}

// TestSavedLayerSuppliedOnce asserts a file the store lacks is supplied once
// however many saved layers list it, and each layer is read at most once: the
// same blob sits at two positions of the manifest, and a third layer lists
// the same file under another name.
func TestSavedLayerSuppliedOnce(t *testing.T) {
	requireTools(t)
	twice := sharedBase()
	other := []octest.File{{Name: "usr/bin/copy", Mode: 0o755, Body: stored("app")}}
	lay := buildLayers(t, twice, other, twice)
	if lay.Layers[0] != lay.Layers[2] {
		t.Fatalf("the manifest does not list one blob twice: %v", lay.Layers)
	}
	root, obj, img := newStores(t)
	want := pullLayout(t, obj, img, lay, lay.Root)
	removeObjects(t, obj, storedDigest("app"))

	rec := newRecorder(t, lay.Dir)
	b, err := Begin(context.Background(), obj, img, rec, lay.Root, lay.Platform, newSlots(2))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
	got, err := b.Finish(context.Background())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !reflect.DeepEqual(b.reused, []int{0, 1, 2}) {
		t.Fatalf("reused layers = %v, want all three positions", b.reused)
	}
	rec.assertOpens(t, []digest.Digest{lay.Layers[0]})
	if !sameBuilt(got, want) {
		t.Fatalf("built mismatch:\n again=%#v\n first=%#v", got, want)
	}
	assertMatchesFreshPull(t, lay, lay.Root, root, got)
}

// TestSavedLayerUnusable asserts a saved layer that records another diff_id
// or blob size, is of an unknown version, or is damaged, is ignored: the
// layer is read as if none were saved, and the scan saves it anew.
func TestSavedLayerUnusable(t *testing.T) {
	requireTools(t)
	layA := buildLayers(t, sharedBase(), releaseLayer("a", true))
	layB := buildLayers(t, sharedBase(), releaseLayer("b", true))
	base, baseSize, diffID := layA.Layers[0], layerSize(t, layA, 0), layA.DiffIDs[0]
	reencode := func(size int64, diffID digest.Digest) func(t *testing.T, data []byte) []byte {
		return func(t *testing.T, data []byte) []byte {
			l, err := rootfs.DecodeLayer(data, 0, baseSize, layA.DiffIDs[0])
			if err != nil {
				t.Fatalf("DecodeLayer: %v", err)
			}
			out, err := rootfs.EncodeLayer(l, size, diffID)
			if err != nil {
				t.Fatalf("EncodeLayer: %v", err)
			}
			return out
		}
	}
	for name, damage := range map[string]func(t *testing.T, data []byte) []byte{
		"another diff_id":   reencode(baseSize, digest.FromString("another layer")),
		"another blob size": reencode(baseSize+1, diffID),
		"unknown version": func(t *testing.T, data []byte) []byte {
			return rewriteSaved(t, data, func(doc []byte) []byte {
				return bytes.Replace(doc, []byte(`"version":1`), []byte(`"version":2`), 1)
			})
		},
		"corrupted": func(_ *testing.T, data []byte) []byte {
			out := append([]byte{}, data...)
			out[len(out)-1] ^= 0xff
			return out
		},
		"empty": func(*testing.T, []byte) []byte { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			root, obj, img := newStores(t)
			pullLayout(t, obj, img, layA, layA.Root)
			data, err := img.GetLayer(base)
			if err != nil {
				t.Fatalf("GetLayer: %v", err)
			}
			damaged := damage(t, data)
			if bytes.Equal(damaged, data) {
				t.Fatal("the saved layer was not changed")
			}
			if err := img.PutLayer(base, damaged); err != nil {
				t.Fatalf("PutLayer: %v", err)
			}

			logs := captureLogs(t)
			rec := newRecorder(t, layB.Dir)
			got, err := Materialize(context.Background(), obj, img, rec, layB.Root, layB.Platform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize(b): %v", err)
			}
			rec.assertOpens(t, layB.Layers)
			if !strings.Contains(logs.String(), "ignoring saved layer") {
				t.Fatalf("no warning about the unusable saved layer:\n%s", logs)
			}
			assertMatchesFreshPull(t, layB, layB.Root, root, got)
			if saved, err := img.GetLayer(base); err != nil || !bytes.Equal(saved, data) {
				t.Fatalf("the scan did not save the layer anew (err %v)", err)
			}
		})
	}
}

// TestSavedLayerIdentity asserts a saved layer stands in only for the blob it
// was saved for, at the size and diff_id it was verified against: an image
// naming that blob with another size or diff_id is rejected as a pull into an
// empty store rejects it, by reading the layer.
func TestSavedLayerIdentity(t *testing.T) {
	requireTools(t)
	for name, edit := range map[string]func(m *ocispec.Manifest, c *ocispec.Image){
		"another diff_id":   func(_ *ocispec.Manifest, c *ocispec.Image) { c.RootFS.DiffIDs[0] = digest.FromString("another layer") },
		"another blob size": func(m *ocispec.Manifest, _ *ocispec.Image) { m.Layers[0].Size++ },
	} {
		t.Run(name, func(t *testing.T) {
			lay := buildLayers(t, sharedBase(), releaseLayer("a", true))
			_, obj, img := newStores(t)
			pullLayout(t, obj, img, lay, lay.Root)

			root := rewriteImage(t, lay, edit)
			rec := newRecorder(t, lay.Dir)
			_, err := Materialize(context.Background(), obj, img, rec, root, lay.Platform, newSlots(2))
			if err == nil {
				t.Fatal("Materialize accepted the image over a saved layer")
			}
			if len(rec.opens) == 0 || !slices.Contains(rec.opens, lay.Layers[0]) {
				t.Fatalf("the layer was not read: opened %v", rec.opens)
			}
			_, freshObj, freshImg := newStores(t)
			if _, freshErr := Materialize(context.Background(), freshObj, freshImg, openLayout(t, lay.Dir), root, lay.Platform, newSlots(2)); freshErr == nil {
				t.Fatal("a pull into an empty store accepted the image")
			}
			if _, err := img.Get(root, lay.Platform); !errors.Is(err, imagestore.ErrNotFound) {
				t.Fatalf("binding lookup after the rejected pull = %v, want not found", err)
			}
		})
	}
}

// TestSavedLayerSealFailure asserts a sealed image whose saved layer was
// altered fails the sealing verification, that the failed pull removes the
// saved layers it reused and no other, and that the next pull, reading those
// layers from the source, builds the sealed image.
func TestSavedLayerSealFailure(t *testing.T) {
	requireTools(t)
	layA := buildLayers(t, sharedBase(), releaseLayer("a", true))
	layB := buildLayers(t, sharedBase(), releaseLayer("b", true))
	base := layA.Layers[0]

	_, freshObj, freshImg := newStores(t)
	unsealed := pullLayout(t, freshObj, freshImg, layB, layB.Root)
	sealedValue, err := ociseal.AnnotationValue(unsealed.FsverityDigest)
	if err != nil {
		t.Fatalf("AnnotationValue: %v", err)
	}
	sealed := rewriteManifestAnnotations(t, layB, map[string]string{ociseal.MergedAnnotation: sealedValue}, nil)

	root, obj, img := newStores(t)
	pullLayout(t, obj, img, layA, layA.Root)
	data, err := img.GetLayer(base)
	if err != nil {
		t.Fatalf("GetLayer: %v", err)
	}
	// The altered saved layer gives one file another mode: every record still
	// decodes, and the image built from it is not the one sealed.
	altered := rewriteSaved(t, data, func(doc []byte) []byte {
		out := bytes.Replace(doc, []byte(`"mode":493`), []byte(`"mode":448`), 1)
		if bytes.Equal(out, doc) {
			t.Fatal("the saved layer holds no record of mode 0755")
		}
		return out
	})
	if err := img.PutLayer(base, altered); err != nil {
		t.Fatalf("PutLayer: %v", err)
	}

	rec := newRecorder(t, layB.Dir)
	_, err = Materialize(context.Background(), obj, img, rec, sealed, layB.Platform, newSlots(2))
	if err == nil || !strings.Contains(err.Error(), "annotation") {
		t.Fatalf("Materialize over the altered saved layer = %v, want an annotation mismatch", err)
	}
	rec.assertOpens(t, []digest.Digest{layB.Layers[1]})
	if _, err := img.Get(sealed, layB.Platform); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("binding lookup after the failed pull = %v, want not found", err)
	}
	assertSaved(t, img, false, base)
	// The layers the failed pull scanned itself, and the ones it did not
	// touch, keep their saved layers.
	assertSaved(t, img, true, layA.Layers[1], layB.Layers[1])

	rec = newRecorder(t, layB.Dir)
	got, err := Materialize(context.Background(), obj, img, rec, sealed, layB.Platform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize again: %v", err)
	}
	rec.assertOpens(t, []digest.Digest{base})
	if got.FsverityDigest != unsealed.FsverityDigest {
		t.Fatalf("fs-verity digest = %s, want the sealed %s", got.FsverityDigest, unsealed.FsverityDigest)
	}
	assertMatchesFreshPull(t, layB, sealed, root, got)
	assertSaved(t, img, true, base)
}

// TestSavedLayerFailureRemovesIt asserts a pull that fails over a saved layer
// in a way the saved layer can have caused removes it, so the failure does
// not repeat: the next pull reads the layer and builds the image a pull into
// an empty store builds. Each altered saved layer still decodes. One makes a
// merge fail, one names a file no layer holds, which fails only after the
// authentic layer was read for it, and one cannot be built into an image.
func TestSavedLayerFailureRemovesIt(t *testing.T) {
	requireTools(t)
	layA := buildLayers(t, sharedBase(), releaseLayer("a", true))
	layB := buildLayers(t, sharedBase(), releaseLayer("b", true))
	base := layA.Layers[0]
	upper := layB.Layers[1]
	for _, c := range []struct {
		name    string
		alter   func(t *testing.T, doc []byte) []byte
		wantErr string
		// opens are the layers the failing pull reads, and again the ones
		// the next pull reads.
		opens, again []digest.Digest
	}{
		{
			// The base fails to merge before the upper layer is opened.
			name:    "hardlink to a missing target",
			alter:   func(t *testing.T, doc []byte) []byte { return retargetSavedLink(t, doc, "usr/bin/app", "usr/bin/nope") },
			wantErr: "target does not exist",
			again:   []digest.Digest{base, upper},
		},
		{
			name: "fabricated stored file",
			alter: func(_ *testing.T, doc []byte) []byte {
				return appendSavedFile(doc, "usr/bin/fabricated", digest.FromString("no layer carries this"))
			},
			wantErr: "supply payload",
			opens:   []digest.Digest{upper, base},
			again:   []digest.Digest{base},
		},
		{
			// The pending files cannot be resolved against the store.
			name: "stored file listed with another size",
			alter: func(_ *testing.T, doc []byte) []byte {
				return appendSavedFile(doc, "usr/bin/resized", storedDigest("app"))
			},
			wantErr: "is listed with sizes",
			opens:   []digest.Digest{upper},
			again:   []digest.Digest{base},
		},
		{
			name: "file in place of a directory",
			alter: func(t *testing.T, doc []byte) []byte {
				dir := fmt.Sprintf(`"name":%q,"mode":%d`, base64.StdEncoding.EncodeToString([]byte("usr/bin")), uint32(os.ModeDir|0o755))
				file := fmt.Sprintf(`"name":%q,"mode":%d`, base64.StdEncoding.EncodeToString([]byte("usr/bin")), 0o644)
				if !bytes.Contains(doc, []byte(dir)) {
					t.Fatalf("the saved layer holds no record %s", dir)
				}
				return bytes.Replace(doc, []byte(dir), []byte(file), 1)
			},
			wantErr: "is not a directory",
			opens:   []digest.Digest{upper},
			again:   []digest.Digest{base},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, obj, img := newStores(t)
			pullLayout(t, obj, img, layA, layA.Root)
			data, err := img.GetLayer(base)
			if err != nil {
				t.Fatalf("GetLayer: %v", err)
			}
			if err := img.PutLayer(base, rewriteSaved(t, data, func(doc []byte) []byte { return c.alter(t, doc) })); err != nil {
				t.Fatalf("PutLayer: %v", err)
			}

			rec := newRecorder(t, layB.Dir)
			_, err = Materialize(context.Background(), obj, img, rec, layB.Root, layB.Platform, newSlots(2))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Materialize over the altered saved layer = %v, want %q", err, c.wantErr)
			}
			rec.assertOpens(t, c.opens)
			assertSaved(t, img, false, base)
			// A layer the pull did not reuse keeps its saved layer.
			assertSaved(t, img, true, layA.Layers[1])
			assertSaved(t, img, slices.Contains(c.opens, upper), upper)
			assertNoTemps(t, obj)

			rec = newRecorder(t, layB.Dir)
			got, err := Materialize(context.Background(), obj, img, rec, layB.Root, layB.Platform, newSlots(2))
			if err != nil {
				t.Fatalf("Materialize again: %v", err)
			}
			rec.assertOpens(t, c.again)
			assertMatchesFreshPull(t, layB, layB.Root, root, got)
			assertSaved(t, img, true, base, upper)
		})
	}

	// A build that is only ever ingested and closed meets the failed merge in
	// Ingest, and removes the saved layer there.
	t.Run("ingest without finish", func(t *testing.T) {
		_, obj, img := newStores(t)
		pullLayout(t, obj, img, layA, layA.Root)
		data, err := img.GetLayer(base)
		if err != nil {
			t.Fatalf("GetLayer: %v", err)
		}
		altered := rewriteSaved(t, data, func(doc []byte) []byte { return retargetSavedLink(t, doc, "usr/bin/app", "usr/bin/nope") })
		if err := img.PutLayer(base, altered); err != nil {
			t.Fatalf("PutLayer: %v", err)
		}
		b, err := Begin(context.Background(), obj, img, openLayout(t, layB.Dir), layB.Root, layB.Platform, newSlots(2))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		first := b.Ingest(context.Background(), 0)
		if first == nil || !strings.Contains(first.Error(), "target does not exist") {
			t.Fatalf("Ingest(0) over the altered saved layer = %v, want the failed merge", first)
		}
		if err := b.Close(); !errors.Is(err, first) {
			t.Fatalf("Close = %v, want %v", err, first)
		}
		assertSaved(t, img, false, base)
	})
}

// TestSavedLayerMergeFailureScope asserts a failed merge removes the saved
// layers merged up to and including the failing layer, and no saved layer
// decoded ahead of it: that one was never merged, so it cannot be at fault,
// and an image that keeps failing must not keep evicting it.
func TestSavedLayerMergeFailureScope(t *testing.T) {
	requireTools(t)
	// A layer that scans cleanly and cannot be merged.
	unmergeable := []octest.File{{Name: "opt", Dir: true}, {Name: "opt/link", Link: "opt/nope"}}
	seed := buildLayers(t, sharedBase())
	sound := seed.Layers[0]

	t.Run("saved layer above the failing one", func(t *testing.T) {
		lay := buildLayers(t, unmergeable, sharedBase())
		if lay.Layers[1] != sound {
			t.Fatalf("the image does not share the saved layer: %s and %s", lay.Layers[1], sound)
		}
		_, obj, img := newStores(t)
		pullLayout(t, obj, img, seed, seed.Root)

		// The lower layer is held in its scan until the upper one has been
		// decoded from its saved layer, ahead of the merge.
		src := newGatedSource(openLayout(t, lay.Dir), lay.Layers[0])
		b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, newSlots(2))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer b.Close() //nolint:errcheck // a failure is reported by the call that hit it
		finished := make(chan error, 1)
		go func() {
			_, err := b.Finish(context.Background())
			finished <- err
		}()
		<-src.entered
		deadline := time.Now().Add(10 * time.Second)
		for {
			b.mu.Lock()
			ahead := slices.Contains(b.reused, 1)
			b.mu.Unlock()
			if ahead {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the upper layer was not decoded ahead of the merge")
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(src.release)
		if err := <-finished; err == nil || !strings.Contains(err.Error(), "target does not exist") {
			t.Fatalf("Finish = %v, want the failed merge", err)
		}
		assertSaved(t, img, true, sound)

		// The scan saved the failing layer, so the next pull merges it from
		// its saved layer: that one goes, the one above it still stays.
		assertSaved(t, img, true, lay.Layers[0])
		_, err = Materialize(context.Background(), obj, img, openLayout(t, lay.Dir), lay.Root, lay.Platform, newSlots(2))
		if err == nil || !strings.Contains(err.Error(), "target does not exist") {
			t.Fatalf("Materialize again = %v, want the failed merge", err)
		}
		assertSaved(t, img, false, lay.Layers[0])
		assertSaved(t, img, true, sound)
	})

	t.Run("saved layer below the failing one", func(t *testing.T) {
		lay := buildLayers(t, sharedBase(), unmergeable)
		_, obj, img := newStores(t)
		pullLayout(t, obj, img, seed, seed.Root)
		_, err := Materialize(context.Background(), obj, img, openLayout(t, lay.Dir), lay.Root, lay.Platform, newSlots(2))
		if err == nil || !strings.Contains(err.Error(), "target does not exist") {
			t.Fatalf("Materialize = %v, want the failed merge", err)
		}
		assertSaved(t, img, false, sound)
	})
}

// TestSavedLayerSourceFailureKeepsIt asserts a failure to read a layer from
// the source, for a file its saved layer left to fetch, removes no saved
// layer: nothing suggests one of them is at fault.
func TestSavedLayerSourceFailureKeepsIt(t *testing.T) {
	requireTools(t)
	lay := buildLayers(t, sharedBase(), releaseLayer("a", true))
	root, obj, img := newStores(t)
	pullLayout(t, obj, img, lay, lay.Root)
	removeObjects(t, obj, storedDigest("app"))

	rec := newRecorder(t, lay.Dir)
	rec.failOpen = map[digest.Digest]error{lay.Layers[0]: errInjected}
	if _, err := Materialize(context.Background(), obj, img, rec, lay.Root, lay.Platform, newSlots(2)); !errors.Is(err, errInjected) {
		t.Fatalf("Materialize with the layer unreadable = %v, want the injected failure", err)
	}
	assertSaved(t, img, true, lay.Layers...)
	assertNoTemps(t, obj)

	got, err := Materialize(context.Background(), obj, img, openLayout(t, lay.Dir), lay.Root, lay.Platform, newSlots(2))
	if err != nil {
		t.Fatalf("Materialize once the layer is readable: %v", err)
	}
	assertMatchesFreshPull(t, lay, lay.Root, root, got)
}

// TestSavedLayerNotSavedOnFailure asserts a layer that fails verification is
// not saved, and that a layer that cannot be saved does not fail the pull.
func TestSavedLayerNotSavedOnFailure(t *testing.T) {
	requireTools(t)
	t.Run("corrupt layer", func(t *testing.T) {
		lay := buildLayers(t, sharedBase(), releaseLayer("a", true))
		blobs := readLayoutBlobs(t, lay.Dir)
		corruptBlob(blobs, lay.Layers[1])
		_, obj, img := newStores(t)
		_, err := Materialize(context.Background(), obj, img, blobs, lay.Root, lay.Platform, newSlots(1))
		skipIfNoVerity(t, err)
		if err == nil {
			t.Fatal("Materialize accepted a corrupt layer")
		}
		assertSaved(t, img, true, lay.Layers[0])
		assertSaved(t, img, false, lay.Layers[1])
	})
	t.Run("unwritable layers directory", func(t *testing.T) {
		lay := buildLayers(t, sharedBase(), releaseLayer("a", true))
		root, obj, img := newStores(t)
		if err := os.WriteFile(filepath.Join(root, "layers"), nil, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		logs := captureLogs(t)
		got := pullLayout(t, obj, img, lay, lay.Root)
		if !strings.Contains(logs.String(), "failed to save layer") {
			t.Fatalf("no warning about the layer that was not saved:\n%s", logs)
		}
		if err := os.Remove(filepath.Join(root, "layers")); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		assertSaved(t, img, false, lay.Layers...)
		assertMatchesFreshPull(t, lay, lay.Root, root, got)
	})
}

// TestSavedLayerFallbackInterrupted asserts what ends the read of a layer for
// its saved layer's missing files. A cancelled Finish context ends it: Finish
// returns the cancellation, holding no slot, temp or binding, and a new pull
// succeeds. Close does not: it waits for the read and for the image and its
// binding to be written.
func TestSavedLayerFallbackInterrupted(t *testing.T) {
	requireTools(t)
	lay := buildLayers(t, sharedBase(), releaseLayer("a", true))
	base := lay.Layers[0]

	t.Run("cancelled context", func(t *testing.T) {
		root, obj, img := newStores(t)
		pullLayout(t, obj, img, lay, lay.Root)
		removeObjects(t, obj, storedDigest("app"))
		if err := img.Delete(lay.Root, lay.Platform); err != nil {
			t.Fatalf("Delete binding: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec := newRecorder(t, lay.Dir)
		rec.onOpen = func(d digest.Digest) {
			if d == base {
				cancel()
			}
		}
		slots := newSlots(2)
		b, err := Begin(context.Background(), obj, img, rec, lay.Root, lay.Platform, slots)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := b.Finish(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Finish = %v, want context.Canceled", err)
		}
		assertSaved(t, img, true, lay.Layers...)
		if err := b.Close(); err != nil {
			t.Fatalf("Close after the cancelled Finish = %v, want nil", err)
		}
		if len(slots) != 0 {
			t.Fatalf("%d slots still held after the cancelled Finish", len(slots))
		}
		assertNoTemps(t, obj)
		if _, err := img.Get(lay.Root, lay.Platform); !errors.Is(err, imagestore.ErrNotFound) {
			t.Fatalf("binding lookup after the cancelled Finish = %v, want not found", err)
		}
		got, err := Materialize(context.Background(), obj, img, openLayout(t, lay.Dir), lay.Root, lay.Platform, slots)
		if err != nil {
			t.Fatalf("Materialize after the cancelled Finish: %v", err)
		}
		assertMatchesFreshPull(t, lay, lay.Root, root, got)
	})

	t.Run("close", func(t *testing.T) {
		root, obj, img := newStores(t)
		pullLayout(t, obj, img, lay, lay.Root)
		removeObjects(t, obj, storedDigest("app"))
		if err := img.Delete(lay.Root, lay.Platform); err != nil {
			t.Fatalf("Delete binding: %v", err)
		}

		src := newGatedSource(openLayout(t, lay.Dir), base)
		b, err := Begin(context.Background(), obj, img, src, lay.Root, lay.Platform, newSlots(2))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		type outcome struct {
			built Built
			err   error
		}
		finished := make(chan outcome, 1)
		go func() {
			built, err := b.Finish(context.Background())
			finished <- outcome{built, err}
		}()
		// The base is opened only to supply the missing file, so the gate is
		// reached inside Finish's tail.
		<-src.entered
		closed := make(chan error, 1)
		go func() { closed <- b.Close() }()
		select {
		case err := <-closed:
			t.Fatalf("Close returned %v while Finish was reading a layer", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(src.release)
		out := <-finished
		if out.err != nil {
			t.Fatalf("Finish: %v", out.err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		if _, err := img.Get(lay.Root, lay.Platform); err != nil {
			t.Fatalf("binding after Finish: %v", err)
		}
		assertMatchesFreshPull(t, lay, lay.Root, root, out.built)
	})
}

// layerSize returns the size lay's manifest declares for layer i.
func layerSize(t *testing.T, lay octest.Layout, i int) int64 {
	t.Helper()
	return readFixtureJSON[ocispec.Manifest](t, blobPath(lay.Dir, lay.Manifest)).Layers[i].Size
}

// rewriteImage rewrites lay's manifest and config through edit, writes both
// under their new digests, and returns the new manifest digest as the pinned
// root.
func rewriteImage(t *testing.T, lay octest.Layout, edit func(m *ocispec.Manifest, c *ocispec.Image)) digest.Digest {
	t.Helper()
	manifest := readFixtureJSON[ocispec.Manifest](t, blobPath(lay.Dir, lay.Manifest))
	config := readFixtureJSON[ocispec.Image](t, blobPath(lay.Dir, lay.Config))
	edit(&manifest, &config)
	configDesc := writeFixtureJSON(t, lay.Dir, config, ocispec.MediaTypeImageConfig)
	manifest.Config.Digest, manifest.Config.Size = configDesc.Digest, configDesc.Size
	return writeFixtureJSON(t, lay.Dir, manifest, ocispec.MediaTypeImageManifest).Digest
}

// retargetSavedLink points the hardlinks a saved layer records to from at to.
func retargetSavedLink(t *testing.T, doc []byte, from, to string) []byte {
	t.Helper()
	target := func(name string) []byte {
		return fmt.Appendf(nil, `"target":%q`, base64.StdEncoding.EncodeToString([]byte(name)))
	}
	if !bytes.Contains(doc, target(from)) {
		t.Fatalf("the saved layer records no hardlink to %s", from)
	}
	return bytes.ReplaceAll(doc, target(from), target(to))
}

// appendSavedFile adds to a saved layer the record of a stored file of 100
// bytes with the given content digest, which the layer does not carry.
func appendSavedFile(doc []byte, name string, d digest.Digest) []byte {
	return fmt.Appendf(append([]byte{}, doc...), `{"kind":"node","name":%q,"mode":%d,"digest":%q,"size":100}`+"\n",
		base64.StdEncoding.EncodeToString([]byte(name)), 0o644, d)
}

// rewriteSaved decompresses a saved layer, passes its JSON through edit, and
// compresses the result again, so the frame and its checksum stay sound.
func rewriteSaved(t *testing.T, data []byte, edit func(doc []byte) []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	doc, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatalf("decompress saved layer: %v", err)
	}
	var out bytes.Buffer
	encoder, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(edit(doc)); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
