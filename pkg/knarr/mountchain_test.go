// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/octest"
)

func TestResolveChainHit(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	lay := seedChainImage(t, root, "app@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
		{Files: []octest.File{{Name: "b", Mode: 0o644, Body: "two"}}},
	}, nil)

	want := chainIDs(lay.DiffIDs)
	info, err := eng.resolveChain(context.Background(), want[len(want)-1].String())
	if err != nil {
		t.Fatalf("resolveChain: %v", err)
	}
	if info.Root != lay.Root {
		t.Fatalf("Root = %s, want %s", info.Root, lay.Root)
	}
}

func TestResolveChainMiss(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	seedChainImage(t, root, "app@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
	}, nil)

	absent := digest.SHA256.FromString("no image ends here").String()
	_, err := eng.resolveChain(context.Background(), absent)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveChain = %v, want an ErrNotFound match", err)
	}
}

func TestResolveChainProperPrefixIsNotFound(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	lay := seedChainImage(t, root, "app@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
		{Files: []octest.File{{Name: "b", Mode: 0o644, Body: "two"}}},
	}, nil)

	// The first layer's chain ID is a proper prefix of the two-layer chain; only
	// the final chain ID identifies the whole image.
	prefix := chainIDs(lay.DiffIDs)[0].String()
	_, err := eng.resolveChain(context.Background(), prefix)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveChain(prefix) = %v, want an ErrNotFound match", err)
	}
}

func TestResolveChainSharedChainReturnsFirstMatch(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	// Two stored images with identical layers but different history: same
	// diff_ids (hence the same final chain ID), distinct root digests. The scan
	// visits refs in sorted order, so the "aaa" image is the first match.
	layers := []octest.LayerSpec{{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}}}
	first := seedChainImage(t, root, "aaa@%s", layers, []ocispec.History{{CreatedBy: "first"}})
	second := seedChainImage(t, root, "zzz@%s", layers, []ocispec.History{{CreatedBy: "second"}})
	if first.Root == second.Root {
		t.Fatal("fixture images share a root digest; they must differ to test first-match")
	}

	want := chainIDs(first.DiffIDs)
	info, err := eng.resolveChain(context.Background(), want[len(want)-1].String())
	if err != nil {
		t.Fatalf("resolveChain: %v", err)
	}
	if info.Root != first.Root {
		t.Fatalf("Root = %s, want the first-by-ref image %s", info.Root, first.Root)
	}
}

func TestResolveChainSkipsZeroLayerImage(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	// A zero-layer image sorts first but has no final chain ID; the scan must
	// skip it and still resolve the healthy image.
	seedZeroLayerImage(t, root, "aaa@%s")
	healthy := seedChainImage(t, root, "bbb@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
	}, nil)

	want := chainIDs(healthy.DiffIDs)
	info, err := eng.resolveChain(context.Background(), want[len(want)-1].String())
	if err != nil {
		t.Fatalf("resolveChain: %v", err)
	}
	if info.Root != healthy.Root {
		t.Fatalf("Root = %s, want %s", info.Root, healthy.Root)
	}
}

func TestResolveChainRejectsInvalidChainID(t *testing.T) {
	eng := newChainEngine(t, t.TempDir())
	_, err := eng.resolveChain(context.Background(), "sha256:nothex")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveChain(malformed) = %v, want a digest parse error, not ErrNotFound", err)
	}
}

// TestResolveChainPoisonedNeighborDoesNotBlockMatch: a stored record that fails
// to inspect (for a reason other than not-found) must not hide a healthy image
// that comes after it in the scan.
func TestResolveChainPoisonedNeighborDoesNotBlockMatch(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	seedPoisonImage(t, root, "aaa@%s")
	healthy := seedChainImage(t, root, "bbb@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
	}, nil)

	want := chainIDs(healthy.DiffIDs)
	info, err := eng.resolveChain(context.Background(), want[len(want)-1].String())
	if err != nil {
		t.Fatalf("resolveChain: %v", err)
	}
	if info.Root != healthy.Root {
		t.Fatalf("Root = %s, want %s", info.Root, healthy.Root)
	}
}

// TestResolveChainUnparseablePlatformRecordDoesNotBlockMatch: a ref record that
// refstore accepts (valid schema and refKey, a non-empty platforms entry) but
// whose platform string is not an os/arch pair must not hide a healthy image.
// The resolver enumerates records by their resolved digest without parsing the
// platform, so such a record is inspected and skipped like any other unreadable
// one instead of aborting the scan.
func TestResolveChainUnparseablePlatformRecordDoesNotBlockMatch(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	seedPoisonImagePlatform(t, root, "aaa@%s", "garbage")
	healthy := seedChainImage(t, root, "bbb@%s", []octest.LayerSpec{
		{Files: []octest.File{{Name: "a", Mode: 0o644, Body: "one"}}},
	}, nil)

	want := chainIDs(healthy.DiffIDs)
	info, err := eng.resolveChain(context.Background(), want[len(want)-1].String())
	if err != nil {
		t.Fatalf("resolveChain: %v", err)
	}
	if info.Root != healthy.Root {
		t.Fatalf("Root = %s, want %s", info.Root, healthy.Root)
	}
}

// TestResolveChainNoMatchSurfacesInspectionErrors: when nothing matches, the
// error is an ErrNotFound match and also carries the inspection failure, named
// by the record it came from, so a corrupt store is not mistaken for a plain
// miss.
func TestResolveChainNoMatchSurfacesInspectionErrors(t *testing.T) {
	root := t.TempDir()
	eng := newChainEngine(t, root)
	poison := seedPoisonImage(t, root, "app@%s")

	absent := digest.SHA256.FromString("no image ends here").String()
	_, err := eng.resolveChain(context.Background(), absent)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolveChain = %v, want an ErrNotFound match", err)
	}
	if !strings.Contains(err.Error(), poison.String()) {
		t.Fatalf("resolveChain error = %v, want the poisoned record %s named", err, poison)
	}
}

// TestMountChainFromPulledImageE2E resolves a pulled image by its chain identity
// and mounts it, exercising the full resolve-and-mount path. It needs a
// verity-capable store and a real composefs mount, so it skips outside the
// privileged dev container the same way the reference-based mount test does.
func TestMountChainFromPulledImageE2E(t *testing.T) {
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

	info, err := eng.Inspect(context.Background(), lay.Ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	chainID := info.Layers[len(info.Layers)-1].ChainID.String()

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

	err = eng.MountChain(context.Background(), chainID, target)
	if skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("MountChain: %v", err)
	}
	mounted = true

	data, err := os.ReadFile(filepath.Join(target, "etc", "config.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello from knarr\n" {
		t.Fatalf("mounted file = %q", data)
	}
}

// newChainEngine builds an engine on root and returns its concrete type so the
// resolver tests can drive the unexported resolveChain directly.
func newChainEngine(t *testing.T, root string) *engine {
	t.Helper()
	eng, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng.(*engine)
}

// chainIDs returns the cumulative chain IDs for an ordered list of diff_ids,
// without mutating the input (identity.ChainIDs writes back into its argument).
func chainIDs(diffIDs []digest.Digest) []digest.Digest {
	chain := append([]digest.Digest(nil), diffIDs...)
	identity.ChainIDs(chain)
	return chain
}

// seedChainImage builds a host-platform image with the given layers and history,
// stores its metadata blobs and an auxiliary ref and binding, and returns the
// built layout. refFmt is a %s ref template the root digest is substituted into
// (so the scan order is controllable by choosing the ref prefix). The metadata
// blobs are written straight to the object store rather than through a pull, so
// the resolver runs without fs-verity or a network.
func seedChainImage(t *testing.T, root, refFmt string, layers []octest.LayerSpec, history []ocispec.History) octest.Layout {
	t.Helper()
	lay, err := octest.BuildImage(t.TempDir(), octest.ImageSpec{
		Platform: hostPlatform(),
		Layers:   layers,
		History:  history,
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	for _, d := range []digest.Digest{lay.Root, lay.Manifest, lay.Config} {
		data, err := os.ReadFile(filepath.Join(lay.Dir, "blobs", "sha256", d.Hex())) // #nosec G304 -- test path from a digest it wrote
		if err != nil {
			t.Fatalf("read blob %s: %v", d, err)
		}
		seedObject(t, root, d, data)
	}
	ref := strings.Replace(refFmt, "%s", lay.Root.String(), 1)
	putEngineRef(t, root, ref, lay.Root, lay.RootSize, hostPlatform())
	putEngineBinding(t, root, lay.Root, hostPlatform(), digest.SHA256.FromString("cfs-"+ref))
	return lay
}

// seedZeroLayerImage stores a valid host-platform image that carries no layers,
// so it inspects cleanly but has no final chain ID for the resolver to match.
func seedZeroLayerImage(t *testing.T, root, refFmt string) {
	t.Helper()
	p := hostPlatform()
	configJSON, err := json.Marshal(ocispec.Image{Platform: p, RootFS: ocispec.RootFS{Type: "layers"}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDigest := digest.SHA256.FromBytes(configJSON)
	manifestJSON, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configJSON)),
		},
		Layers: []ocispec.Descriptor{},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := digest.SHA256.FromBytes(manifestJSON)
	seedObject(t, root, configDigest, configJSON)
	seedObject(t, root, manifestDigest, manifestJSON)
	ref := strings.Replace(refFmt, "%s", manifestDigest.String(), 1)
	putEngineRef(t, root, ref, manifestDigest, int64(len(manifestJSON)), p)
	putEngineBinding(t, root, manifestDigest, p, digest.SHA256.FromString("cfs-"+ref))
}

// seedPoisonImage stores a ref whose root "metadata" blob is not valid OCI JSON,
// so inspecting it fails with a parse error rather than a not-found result. It
// returns the resolved digest, which the no-match diagnostic names.
func seedPoisonImage(t *testing.T, root, refFmt string) digest.Digest {
	t.Helper()
	p := hostPlatform()
	return seedPoisonImagePlatform(t, root, refFmt, ocilayout.PlatformString(&p))
}

// seedPoisonImagePlatform is seedPoisonImage with an explicit platform string,
// so a record with an unparseable platform can be stored to prove the resolver
// enumerates records without parsing their platform. refstore only requires the
// platforms entry to be non-empty, so it accepts an arbitrary string.
func seedPoisonImagePlatform(t *testing.T, root, refFmt, platform string) digest.Digest {
	t.Helper()
	garbage := []byte("not a manifest")
	d := digest.SHA256.FromBytes(garbage)
	seedObject(t, root, d, garbage)
	ref := strings.Replace(refFmt, "%s", d.String(), 1)
	refStore := newRefStore(t, root)
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    d,
		Size:      int64(len(garbage)),
	}
	err := refStore.Put(ref, desc, desc, time.Unix(0, 0).UTC(), []string{platform})
	if err != nil {
		t.Fatalf("Put ref: %v", err)
	}
	return d
}

// seedObject writes data straight to its content-addressed object path, without
// the fs-verity sealing a pull performs, so metadata is seedable on any host.
func seedObject(t *testing.T, root string, d digest.Digest, data []byte) {
	t.Helper()
	path := octest.ObjectPath(root, d)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil { // #nosec G304 G703 -- test path from a digest it wrote
		t.Fatalf("WriteFile: %v", err)
	}
}
