// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/octest"
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
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from ocicfs\n"},
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
	if want := int64(len("hello from ocicfs\n")); info.RootfsSize != want {
		t.Fatalf("RootfsSize = %d, want %d (the fixture file's content bytes)", info.RootfsSize, want)
	}
	if len(info.Layers) != 1 {
		t.Fatalf("Layers len = %d, want 1", len(info.Layers))
	}
	layer := info.Layers[0]
	if layer.DiffID != lay.DiffIDs[0] || layer.ChainID != lay.DiffIDs[0] {
		t.Fatalf("layer diff/chain = %s/%s, want both %s (single layer)", layer.DiffID, layer.ChainID, lay.DiffIDs[0])
	}
	if layer.MediaType != ocispec.MediaTypeImageLayerGzip || layer.Size <= 0 {
		t.Fatalf("layer descriptor = %q/%d, want tar+gzip with positive size", layer.MediaType, layer.Size)
	}
	if layer.CreatedBy != "" {
		t.Fatalf("layer CreatedBy = %q, want empty (fixture has no history)", layer.CreatedBy)
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

// resolvedFixture builds an ocichain.Resolved directly with n layers and the
// given config history; layerTable consumes only Manifest.Layers, DiffIDs,
// and Config.History, all of which are exported.
func resolvedFixture(n int, history []ocispec.History) ocichain.Resolved {
	var res ocichain.Resolved
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("layer-%d", i)
		res.Manifest.Layers = append(res.Manifest.Layers, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    digest.SHA256.FromString("blob-" + name),
			Size:      int64(100 + i),
		})
		res.DiffIDs = append(res.DiffIDs, digest.SHA256.FromString("diff-"+name))
	}
	res.Config.History = history
	return res
}

func TestLayerTableCorrelationAndChainIDs(t *testing.T) {
	res := resolvedFixture(3, []ocispec.History{
		{CreatedBy: "ADD rootfs.tar /"},
		{CreatedBy: "ENV A=1", EmptyLayer: true},
		{CreatedBy: "RUN apk add nginx"},
		{CreatedBy: "LABEL x=y", EmptyLayer: true},
		{CreatedBy: "COPY conf /etc"},
	})

	layers := layerTable(res)
	if len(layers) != 3 {
		t.Fatalf("layers = %d, want 3", len(layers))
	}

	// Chain-ID oracle, hand-computed per the OCI definition:
	// chain(parent, diff) = sha256(parent + " " + diff).
	chain := func(parent, diff digest.Digest) digest.Digest {
		return digest.SHA256.FromString(string(parent) + " " + string(diff))
	}
	wantChains := []digest.Digest{res.DiffIDs[0]}
	wantChains = append(wantChains, chain(wantChains[0], res.DiffIDs[1]))
	wantChains = append(wantChains, chain(wantChains[1], res.DiffIDs[2]))
	wantCreated := []string{"ADD rootfs.tar /", "RUN apk add nginx", "COPY conf /etc"}

	for i, l := range layers {
		want := res.Manifest.Layers[i]
		if l.Digest != want.Digest || l.MediaType != want.MediaType || l.Size != want.Size {
			t.Errorf("layer %d descriptor = %s/%q/%d, want %s/%q/%d", i, l.Digest, l.MediaType, l.Size, want.Digest, want.MediaType, want.Size)
		}
		if l.DiffID != res.DiffIDs[i] {
			t.Errorf("layer %d DiffID = %s, want %s", i, l.DiffID, res.DiffIDs[i])
		}
		if l.ChainID != wantChains[i] {
			t.Errorf("layer %d ChainID = %s, want %s", i, l.ChainID, wantChains[i])
		}
		if l.CreatedBy != wantCreated[i] {
			t.Errorf("layer %d CreatedBy = %q, want %q", i, l.CreatedBy, wantCreated[i])
		}
	}
}

func TestLayerTableDoesNotMutateDiffIDs(t *testing.T) {
	res := resolvedFixture(3, nil)
	orig := append([]digest.Digest(nil), res.DiffIDs...)

	_ = layerTable(res)

	for i := range orig {
		if res.DiffIDs[i] != orig[i] {
			t.Fatalf("DiffIDs[%d] mutated by layerTable: %s, want %s", i, res.DiffIDs[i], orig[i])
		}
	}
}

func TestLayerTableSingleLayerChainIDIsDiffID(t *testing.T) {
	res := resolvedFixture(1, nil)
	layers := layerTable(res)
	if len(layers) != 1 || layers[0].ChainID != res.DiffIDs[0] {
		t.Fatalf("single-layer ChainID = %v, want DiffID %s", layers, res.DiffIDs[0])
	}
}

func TestLayerTableUncorrelatableHistoryLeavesCreatedByEmpty(t *testing.T) {
	for name, history := range map[string][]ocispec.History{
		"absent":             nil,
		"too few non-empty":  {{CreatedBy: "ADD /"}},
		"too many non-empty": {{CreatedBy: "a"}, {CreatedBy: "b"}, {CreatedBy: "c"}},
	} {
		res := resolvedFixture(2, history)
		for i, l := range layerTable(res) {
			if l.CreatedBy != "" {
				t.Errorf("%s: layer %d CreatedBy = %q, want empty", name, i, l.CreatedBy)
			}
		}
	}
}

// TestLayerTableMultiPlatformResolve drives a genuine multi-manifest index
// through ocilayout + ocichain.Resolve and checks the table describes the
// host platform's layers, not the other platform's.
func TestLayerTableMultiPlatformResolve(t *testing.T) {
	host := hostPlatform()
	lay, err := octest.BuildImage(t.TempDir(),
		octest.ImageSpec{
			Platform: host,
			Layers: []octest.LayerSpec{
				{Files: []octest.File{{Name: "base.txt", Mode: 0o644, Body: "host base"}}},
				{Files: []octest.File{{Name: "app.txt", Mode: 0o644, Body: "host app"}}, MediaType: ocispec.MediaTypeImageLayerZstd},
			},
			History: []ocispec.History{
				{CreatedBy: "ADD base /"},
				{CreatedBy: "ENV A=1", EmptyLayer: true},
				{CreatedBy: "ADD app /"},
			},
		},
		octest.ImageSpec{
			Platform: ocispec.Platform{OS: "linux", Architecture: "mips64le"},
			Layers: []octest.LayerSpec{
				{Files: []octest.File{{Name: "other.txt", Mode: 0o644, Body: "other"}}},
			},
		},
	)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	src, err := ocilayout.Open(lay.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	res, err := ocichain.Resolve(context.Background(), src, lay.Root, host)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	layers := layerTable(res)
	if len(layers) != 2 {
		t.Fatalf("layers = %d, want the host image's 2", len(layers))
	}
	for i, l := range layers {
		if l.DiffID != lay.DiffIDs[i] {
			t.Errorf("layer %d DiffID = %s, want host image's %s", i, l.DiffID, lay.DiffIDs[i])
		}
	}
	if layers[1].MediaType != ocispec.MediaTypeImageLayerZstd {
		t.Errorf("layer 1 media type = %q, want zstd", layers[1].MediaType)
	}
	if layers[0].CreatedBy != "ADD base /" || layers[1].CreatedBy != "ADD app /" {
		t.Errorf("createdBy = %q/%q, want the two non-empty history entries", layers[0].CreatedBy, layers[1].CreatedBy)
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
