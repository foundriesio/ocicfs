// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/octest"
	"github.com/foundriesio/knarr/internal/rootfs"
)

func TestParseDigestPinnedRef(t *testing.T) {
	hex256 := strings.Repeat("a", 64)
	want := digest.Digest("sha256:" + hex256)
	tests := []struct {
		name    string
		ref     string
		want    digest.Digest
		wantErr string
	}{
		{"name digest", "registry.example.com/team/app@" + want.String(), want, ""},
		{"bare digest", want.String(), want, ""},
		{"tag only", "registry.example.com/team/app:v1", "", "digest-pinned reference is required"},
		{"sha-prefixed name", "sha-registry.com/app:v1", "", "digest-pinned reference is required"},
		{"empty", "", "", "digest-pinned reference is required"},
		{"wrong algorithm", "sha512:" + strings.Repeat("b", 128), "", "non-sha256"},
		{"malformed", "sha256:bad", "", "invalid digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDigestPinnedRef(tt.ref)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDigestPinnedRef: %v", err)
			}
			if got != tt.want {
				t.Fatalf("digest = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNormalizeRef(t *testing.T) {
	hex256 := strings.Repeat("a", 64)
	bareDigest := "sha256:" + hex256
	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr string
	}{
		{"short name", "ubuntu", "index.docker.io/library/ubuntu:latest", ""},
		{"missing tag", "registry.example.com/team/app", "registry.example.com/team/app:latest", ""},
		{"host with port", "registry.example.com:5000/team/app:v1", "registry.example.com:5000/team/app:v1", ""},
		{"non-docker-hub host", "quay.io/team/app:v1", "quay.io/team/app:v1", ""},
		{"name at digest", "registry.example.com/team/app@" + bareDigest, "registry.example.com/team/app@" + bareDigest, ""},
		{"bare digest passes through", bareDigest, bareDigest, ""},
		{"malformed", "not a ref", "", "normalize ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRef(tt.ref)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRef: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

// mustNormalizeRef computes the canonical refstore key for ref, the same
// transform pull() applies before storing and resolveLocalRef()/remove()
// apply before looking up, so a test can predict the stored form of a ref
// built from a short or unqualified name.
func mustNormalizeRef(t *testing.T, ref string) string {
	t.Helper()
	normalized, err := normalizeRef(ref)
	if err != nil {
		t.Fatalf("normalizeRef(%q): %v", ref, err)
	}
	return normalized
}

// TestPullInspectRemoveDifferentAliasE2E pulls an image under one non-digest
// alias and confirms Inspect and Remove both succeed when called with a
// differently formed but equivalent alias, proving pull and lookup converge
// on the same normalized refstore key rather than keying on the exact string
// typed at pull time. Both aliases omit a digest pin, so resolveLocalRef must
// take its named-ref branch (through e.refs.Get(normalized)) rather than the
// digest-pinned branch, exercising the exact lookup path a normalizing
// admission gate exercises. Neither alias is itself the canonical form the
// image is stored under, so the test fails if normalization is missing on
// either the pull or the lookup side.
func TestPullInspectRemoveDifferentAliasE2E(t *testing.T) {
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

	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	tr := reg.Transport()
	useRemoteDefaultTransport(t, tr)

	// Images on the default registry canonicalize to
	// "index.docker.io/library/<name>:tag" regardless of which host alias or
	// short name names them; push under that canonical host so both a short
	// name and an explicit "docker.io" alias resolve here.
	remoteImage, _ := registryHostImage(t, 1)
	root := pushRegistryImage(t, "index.docker.io", "library/app", remoteImage, tr)

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// "app" is short for "index.docker.io/library/app:latest".
	pullRef := "app"
	if _, err := eng.Pull(context.Background(), pullRef); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// A caller that normalizes refs before lookup (e.g. an admission gate)
	// resolves this image under the explicit "docker.io" host and tag, a
	// different string from "app" that folds to the same canonical form.
	lookupRef := "docker.io/library/app:latest"
	if lookupRef == pullRef {
		t.Fatal("fixture refs must differ to test alias convergence")
	}

	info, err := eng.Inspect(context.Background(), lookupRef)
	if err != nil {
		t.Fatalf("Inspect(%q): %v", lookupRef, err)
	}
	if info.Root != root {
		t.Fatalf("Inspect Root = %s, want %s", info.Root, root)
	}

	if err := eng.Remove(context.Background(), lookupRef); err != nil {
		t.Fatalf("Remove(%q): %v", lookupRef, err)
	}
	if _, err := eng.Inspect(context.Background(), lookupRef); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect after remove: got %v, want ErrNotFound", err)
	}
}

func TestPullRegistryRejectsBareDigestAndMalformedRef(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tt := range []struct {
		ref     string
		wantErr string
	}{
		{"sha256:" + strings.Repeat("a", 64), "bare digest"},
		{"not a ref", "ociremote: parse reference"},
	} {
		_, err = eng.Pull(context.Background(), tt.ref)
		if err == nil {
			t.Fatalf("Pull(%q): expected error", tt.ref)
		}
		if errors.Is(err, ErrNotImplemented) || errors.Is(err, ErrNotFound) {
			t.Fatalf("Pull(%q) error = %v, want parse error only", tt.ref, err)
		}
		if !strings.Contains(err.Error(), tt.wantErr) {
			t.Fatalf("Pull(%q) error = %v, want it to contain %q", tt.ref, err, tt.wantErr)
		}
	}
}

func TestPullRegistryMissingRootNotLocalNotFound(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	useRemoteDefaultTransport(t, reg.Transport())

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "registry.example.com/team/missing@sha256:" + strings.Repeat("d", 64)
	_, err = eng.Pull(context.Background(), ref)
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err == nil {
		t.Fatal("expected missing-root error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("missing remote root must not be local ErrNotFound: %v", err)
	}
	if !strings.Contains(err.Error(), "not present in registry registry.example.com/team/missing") {
		t.Fatalf("error = %v, want ociremote missing-root message", err)
	}
}

func TestPullFromDirE2E(t *testing.T) {
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
		// Over the inline threshold, so it is stored as a CAS object (FileDigest).
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr - a config payload larger than the sixty-four byte inline limit\n"},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: "#!/bin/sh\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	img, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir))
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Ref != lay.Ref {
		t.Fatalf("ref = %q, want %q", img.Ref, lay.Ref)
	}
	if img.Digest != lay.Root.String() {
		t.Fatalf("digest = %q, want %s", img.Digest, lay.Root)
	}
	if img.Size <= 0 {
		t.Fatalf("size = %d, want > 0", img.Size)
	}

	binding := readPullBinding(t, storeRoot, lay.Root, lay.Platform)
	if binding.RootFS.Descriptor.MediaType != rootfs.MediaTypeComposefsImage {
		t.Fatalf("composefs mediaType = %q", binding.RootFS.Descriptor.MediaType)
	}
	if binding.RootFS.Descriptor.Size != img.Size {
		t.Fatalf("binding size = %d, returned image size = %d", binding.RootFS.Descriptor.Size, img.Size)
	}
	if binding.Platform.OS != lay.Platform.OS ||
		binding.Platform.Architecture != lay.Platform.Architecture ||
		binding.Platform.Variant != lay.Platform.Variant {
		t.Fatalf("binding platform = %#v, want %#v", binding.Platform, lay.Platform)
	}
	if binding.RootFS.Fsverity.Mode != "required" || binding.RootFS.Fsverity.Source != "local-measurement" {
		t.Fatalf("binding fsverity = %#v", binding.RootFS.Fsverity)
	}
	assertObjectExists(t, storeRoot, binding.RootFS.Descriptor.Digest)

	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	measured, err := obj.VerityDigest(binding.RootFS.Descriptor.Digest)
	if err != nil {
		t.Fatalf("VerityDigest: %v", err)
	}
	if measured != binding.RootFS.Fsverity.Digest.String() {
		t.Fatalf("fs-verity digest = %q, binding records %s", measured, binding.RootFS.Fsverity.Digest)
	}

	assertObjectExists(t, storeRoot, lay.Root)
	assertObjectExists(t, storeRoot, lay.Manifest)
	assertObjectExists(t, storeRoot, lay.Config)
	assertObjectExists(t, storeRoot, lay.FileDigest)

	wantRef := mustNormalizeRef(t, lay.Ref)
	ref := readPullRef(t, storeRoot, wantRef)
	if ref.Ref != wantRef {
		t.Fatalf("stored ref = %q, want %q", ref.Ref, wantRef)
	}
	if ref.Resolved.Digest != lay.Root {
		t.Fatalf("resolved digest = %s, want %s", ref.Resolved.Digest, lay.Root)
	}
	if ref.Resolved.Size != lay.RootSize {
		t.Fatalf("resolved size = %d, want %d", ref.Resolved.Size, lay.RootSize)
	}
	if len(ref.Platforms) != 1 || ref.Platforms[0] != ocilayout.PlatformString(&lay.Platform) {
		t.Fatalf("platforms = %#v, want %s", ref.Platforms, ocilayout.PlatformString(&lay.Platform))
	}
}

func TestPullRegistryE2ESkipsExistingImage(t *testing.T) {
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

	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	tr := reg.Transport()
	useRemoteDefaultTransport(t, tr)

	remoteImage, wantDiffIDs := registryHostImage(t, 2)
	root := pushRegistryImage(t, "registry.example.com", "team/app", remoteImage, tr)
	ref := "registry.example.com/team/app@" + root.String()
	aliasRef := "registry.example.com/team/alias@" + root.String()

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := eng.Pull(context.Background(), ref)
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}

	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	assertDiffIDs(t, info.DiffIDs, wantDiffIDs)
	if info.RootfsSize <= 0 {
		t.Fatalf("RootfsSize = %d, want > 0 after a real build", info.RootfsSize)
	}

	tagRef := "registry.example.com/team/app:latest"
	tagged, err := eng.Pull(context.Background(), tagRef)
	if err != nil {
		t.Fatalf("Pull by tag: %v", err)
	}
	if tagged.Ref != tagRef || tagged.Digest != root.String() {
		t.Fatalf("tag pull = %#v, want ref %q resolved to %s", tagged, tagRef, root)
	}
	tagRecord := readPullRef(t, storeRoot, tagRef)
	if tagRecord.Ref != tagRef || tagRecord.Resolved.Digest != root {
		t.Fatalf("tag ref record = %#v, want %q -> %s", tagRecord, tagRef, root)
	}

	reg.Close()

	tagInfo, err := eng.Inspect(context.Background(), tagRef)
	if err != nil {
		t.Fatalf("Inspect by pulled tag with closed registry: %v", err)
	}
	if tagInfo.Root != root {
		t.Fatalf("Inspect by tag root = %s, want %s", tagInfo.Root, root)
	}

	if _, err := eng.Pull(context.Background(), tagRef); err == nil {
		t.Fatal("Pull by tag with closed registry: expected resolution error")
	}

	// Simulate a binding written before the rootfs size existed and prove the
	// verified-cache-hit pull below does not backfill it: that would require
	// refetching every layer for a display metric. The size is recorded when
	// the image is next actually built.
	stripBindingSize(t, storeRoot, root, info.Platform)

	second, err := eng.Pull(context.Background(), aliasRef)
	if err != nil {
		t.Fatalf("second Pull with closed registry: %v", err)
	}
	if second.Ref != aliasRef || second.Digest != root.String() || second.Size != first.Size {
		t.Fatalf("second image = %#v, want ref %q digest %s size %d", second, aliasRef, root, first.Size)
	}
	refRecord := readPullRef(t, storeRoot, aliasRef)
	if refRecord.Ref != aliasRef || refRecord.Resolved.Digest != root {
		t.Fatalf("alias ref record = %#v, want %q -> %s", refRecord, aliasRef, root)
	}
	if len(refRecord.Platforms) != 1 || refRecord.Platforms[0] != ocilayout.PlatformString(&info.Platform) {
		t.Fatalf("alias platforms = %#v, want %s", refRecord.Platforms, ocilayout.PlatformString(&info.Platform))
	}

	legacy, err := eng.Inspect(context.Background(), aliasRef)
	if err != nil {
		t.Fatalf("Inspect after cache-hit pull: %v", err)
	}
	if legacy.RootfsSize != 0 {
		t.Fatalf("RootfsSize = %d after a cache-hit pull of a legacy binding, want 0 (no backfill)", legacy.RootfsSize)
	}
}

// TestPullBlobSourceMatchesFromDirE2E pulls one image twice into two fresh
// stores — once from its layout directory, once through a caller-supplied blob
// source serving the same blobs from memory — and requires the resulting object
// and image-binding trees to be byte-identical: the source of the bytes is the
// only thing the option changes. A failing transport is installed for the whole
// test, so either pull touching the network fails it.
func TestPullBlobSourceMatchesFromDirE2E(t *testing.T) {
	layoutStore := verityStoreRoot(t)
	sourceStore := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		canonicalPullFiles("blob source payload past the sixty-four byte inline limit\n")...,
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}

	fromLayout := pullInto(t, layoutStore, lay.Ref, WithFromDir(lay.Dir))
	fromSource := pullInto(t, sourceStore, lay.Ref, WithBlobSource(layoutBlobs(t, lay.Dir)))
	if fromSource.Digest != fromLayout.Digest || fromSource.Size != fromLayout.Size {
		t.Fatalf("blob-source image = %#v, layout image = %#v", fromSource, fromLayout)
	}
	assertTreesEqual(t, layoutStore, sourceStore, "objects")
	assertTreesEqual(t, layoutStore, sourceStore, "images")
	assertRefsEqual(t, layoutStore, sourceStore)
}

// TestPullBlobSourceTagE2E pulls a tagged reference through a blob source with
// the root digest the caller resolved, and requires the tag to be recorded as
// an alias for that digest so a later local lookup resolves it.
func TestPullBlobSourceTagE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		canonicalPullFiles("tagged payload past the sixty-four byte inline limit\n")...,
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}

	ref := "registry.example.com/team/app:v1"
	img := pullInto(t, storeRoot, ref, WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Root))
	if img.Digest != lay.Root.String() {
		t.Fatalf("digest = %q, want %s", img.Digest, lay.Root)
	}

	record := readPullRef(t, storeRoot, mustNormalizeRef(t, ref))
	if record.Resolved.Digest != lay.Root {
		t.Fatalf("ref record = %#v, want %q -> %s", record, ref, lay.Root)
	}

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect(%q): %v", ref, err)
	}
	if info.Root != lay.Root {
		t.Fatalf("Inspect Root = %s, want %s", info.Root, lay.Root)
	}
}

// TestPullBlobSourceResolvedManifestOfIndexPinE2E pulls an index-pinned
// reference with a resolved root naming the platform manifest inside that
// index. The two digests legitimately disagree, and the pull roots at the
// resolved one.
func TestPullBlobSourceResolvedManifestOfIndexPinE2E(t *testing.T) {
	storeRoot := verityStoreRoot(t)
	useRemoteDefaultTransport(t, failTransport{})

	body := "index payload past the sixty-four byte inline limit for the object store\n"
	lay, err := octest.BuildImage(t.TempDir(),
		octest.ImageSpec{Platform: hostPlatform(), Layers: []octest.LayerSpec{{Files: canonicalPullFiles(body)}}},
		octest.ImageSpec{Platform: ocispec.Platform{OS: "plan9", Architecture: "mips"}, Layers: []octest.LayerSpec{{Files: canonicalPullFiles(body)}}},
	)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if lay.Root == lay.Manifest {
		t.Fatal("fixture must pin an index, not a manifest")
	}

	img := pullInto(t, storeRoot, lay.Ref, WithBlobSource(layoutBlobs(t, lay.Dir)), WithResolvedRoot(lay.Manifest))
	if img.Digest != lay.Manifest.String() {
		t.Fatalf("digest = %q, want the resolved manifest %s", img.Digest, lay.Manifest)
	}

	binding := readPullBinding(t, storeRoot, lay.Manifest, lay.Platform)
	if binding.RootFS.Descriptor.Size != img.Size {
		t.Fatalf("binding size = %d, returned image size = %d", binding.RootFS.Descriptor.Size, img.Size)
	}
	record := readPullRef(t, storeRoot, mustNormalizeRef(t, lay.Ref))
	if record.Resolved.Digest != lay.Manifest {
		t.Fatalf("ref record = %#v, want %q -> %s", record, lay.Ref, lay.Manifest)
	}
}

func TestPullBlobSourceOptionErrors(t *testing.T) {
	pinned := "registry.example.com/team/app@sha256:" + strings.Repeat("a", 64)
	tagged := "registry.example.com/team/app:v1"
	root := digest.Digest("sha256:" + strings.Repeat("b", 64))
	tests := []struct {
		name    string
		ref     string
		opts    []PullOption
		wantErr string
	}{
		{"tag without resolved root", tagged, []PullOption{WithBlobSource(&memBlobSource{})}, "digest-pinned reference is required"},
		{"resolved root without blob source", tagged, []PullOption{WithResolvedRoot(root)}, "requires a blob source"},
		{"blob source with layout dir", pinned, []PullOption{WithBlobSource(&memBlobSource{}), WithFromDir(t.TempDir())}, "mutually exclusive"},
		{"malformed resolved root", tagged, []PullOption{WithBlobSource(&memBlobSource{}), WithResolvedRoot("sha256:bad")}, "invalid digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng, err := New(Config{Root: t.TempDir()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = eng.Pull(context.Background(), tt.ref, tt.opts...)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// memBlobSource serves blobs from memory, so a pull through it reaches neither
// a registry nor a layout directory.
type memBlobSource struct {
	blobs map[digest.Digest][]byte
}

func (s *memBlobSource) Open(_ context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	data, ok := s.blobs[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("memBlobSource: blob %s absent: %w", desc.Digest, os.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// failTransport fails every request, so a test can assert an operation is
// fully offline.
type failTransport struct{}

func (failTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected network request: %s %s", req.Method, req.URL)
}

// layoutBlobs reads every blob of the OCI image layout at dir into a
// memBlobSource, keyed by the digest its file name encodes.
func layoutBlobs(t *testing.T, dir string) *memBlobSource {
	t.Helper()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}
	src := &memBlobSource{blobs: make(map[digest.Digest][]byte, len(entries))}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(blobDir, e.Name())) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatalf("read blob %s: %v", e.Name(), err)
		}
		src.blobs[digest.Digest("sha256:"+e.Name())] = data
	}
	return src
}

// verityStoreRoot returns a fresh verity-capable store root, skipping the test
// when the privileged dev container's filesystem or mkcomposefs is missing.
func verityStoreRoot(t *testing.T) string {
	t.Helper()
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
	return storeRoot
}

// pullInto pulls ref into a store rooted at storeRoot through a fresh engine.
func pullInto(t *testing.T, storeRoot, ref string, opts ...PullOption) *Image {
	t.Helper()
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	img, err := eng.Pull(context.Background(), ref, opts...)
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Pull(%q): %v", ref, err)
	}
	return img
}

// assertTreesEqual requires subdirectory sub of two store roots to hold the
// same file paths with the same content.
func assertTreesEqual(t *testing.T, wantRoot, gotRoot, sub string) {
	t.Helper()
	assertSameContents(t, sub, wantRoot, gotRoot,
		treeContents(t, filepath.Join(wantRoot, sub)),
		treeContents(t, filepath.Join(gotRoot, sub)))
}

// assertRefsEqual compares the two stores' ref records with the pulledAt
// timestamp stripped, the one wall-clock field a pull writes.
func assertRefsEqual(t *testing.T, wantRoot, gotRoot string) {
	t.Helper()
	assertSameContents(t, "refs", wantRoot, gotRoot,
		refContents(t, wantRoot), refContents(t, gotRoot))
}

// refContents reads every ref record under root's refs tree with the pulledAt
// field removed, keyed by path relative to that tree.
func refContents(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := treeContents(t, filepath.Join(root, "refs"))
	for path, data := range out {
		var rec map[string]any
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("decode refs/%s: %v", path, err)
		}
		delete(rec, "pulledAt")
		stripped, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("re-encode refs/%s: %v", path, err)
		}
		out[path] = stripped
	}
	return out
}

func assertSameContents(t *testing.T, sub, wantRoot, gotRoot string, want, got map[string][]byte) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s is empty in %s", sub, wantRoot)
	}
	for path, data := range want {
		other, ok := got[path]
		if !ok {
			t.Fatalf("%s/%s missing from %s", sub, path, gotRoot)
		}
		if !bytes.Equal(data, other) {
			t.Fatalf("%s/%s differs between the two stores", sub, path)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Fatalf("%s/%s present only in %s", sub, path, gotRoot)
		}
	}
}

// treeContents reads every regular file under dir, keyed by its path relative
// to dir.
func treeContents(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	out := make(map[string][]byte, len(files))
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel)) // #nosec G304 -- test reads a store it just wrote
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		out[rel] = data
	}
	return out
}

// TestVerifiedPulledImageRejectsTamperedSealingAnnotations pins the cached-path
// fail-closed behavior for a canonical image: a merged or config sealing
// annotation that disagrees with the measured content propagates as an error
// rather than a silent cache miss.
func TestVerifiedPulledImageRejectsTamperedSealingAnnotations(t *testing.T) {
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		canonicalPullFiles("cached image payload past the sixty-four byte inline limit\n")...,
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	obj, imgStore, binding := pullForSealingTest(t, lay)

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
			identity := rebindManifestAnnotations(t, obj, imgStore, lay, binding, tt.manifestAnn, tt.configAnn)
			_, ok, err := verifiedPulledImage(context.Background(), obj, imgStore, identity, lay.Platform)
			if err == nil || !strings.Contains(err.Error(), "annotation") {
				t.Fatalf("verifiedPulledImage error = %v, want annotation failure", err)
			}
			if ok {
				t.Fatal("verifiedPulledImage accepted tampered annotation")
			}
		})
	}
}

// TestVerifiedPulledImageNonCanonicalSealingPolicy pins the cached-path rule for
// a non-canonical image: a merged-EROFS annotation under any suffix key
// propagates as an error, while the same image with no such annotation stays a
// valid cache hit.
func TestVerifiedPulledImageNonCanonicalSealingPolicy(t *testing.T) {
	body := "a payload larger than the sixty-four byte inline limit so it becomes an object\n"

	t.Run("sealed rejected", func(t *testing.T) {
		lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
			octest.File{Name: "etc/config", Mode: 0o644, Body: body},
		)
		if err != nil {
			t.Fatalf("BuildSingleLayerGzip: %v", err)
		}
		obj, imgStore, binding := pullForSealingTest(t, lay)
		if binding.Canonical {
			t.Fatal("divergent binding recorded canonical = true")
		}
		identity := rebindManifestAnnotations(t, obj, imgStore, lay, binding,
			map[string]string{"composefs.merged.erofs.v1.fsverity-sha512-12": strings.Repeat("a", 64)}, nil)
		_, ok, err := verifiedPulledImage(context.Background(), obj, imgStore, identity, lay.Platform)
		if err == nil || !strings.Contains(err.Error(), "non-canonical") {
			t.Fatalf("verifiedPulledImage error = %v, want non-canonical failure", err)
		}
		if ok {
			t.Fatal("verifiedPulledImage accepted a sealed non-canonical image")
		}
	})

	t.Run("unsealed accepted", func(t *testing.T) {
		lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
			octest.File{Name: "etc/config", Mode: 0o644, Body: body},
		)
		if err != nil {
			t.Fatalf("BuildSingleLayerGzip: %v", err)
		}
		obj, imgStore, binding := pullForSealingTest(t, lay)
		if binding.Canonical {
			t.Fatal("divergent binding recorded canonical = true")
		}
		identity := ocispec.Descriptor{Digest: lay.Root, Size: lay.RootSize}
		_, ok, err := verifiedPulledImage(context.Background(), obj, imgStore, identity, lay.Platform)
		if err != nil {
			t.Fatalf("verifiedPulledImage: %v", err)
		}
		if !ok {
			t.Fatal("verifiedPulledImage missed an unsealed non-canonical cache hit")
		}
	})
}

// canonicalPullFiles is a standard-layout single-layer tree — /usr and its
// intermediate directories are explicit, so the construction is canonical.
func canonicalPullFiles(body string) []octest.File {
	return []octest.File{
		{Name: "usr", Dir: true},
		{Name: "usr/bin", Dir: true},
		{Name: "usr/bin/app", Mode: 0o755, Body: body},
	}
}

// pullForSealingTest pulls lay from its layout directory into a fresh
// verity-capable store and returns the store handles and the recorded binding.
func pullForSealingTest(t *testing.T, lay octest.Layout) (*objectstore.Store, *imagestore.Store, imagestore.Binding) {
	t.Helper()
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
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); err != nil {
		if devtest.IsVerityUnsupported(err) {
			t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
		}
		t.Fatalf("Pull: %v", err)
	}
	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	imgStore, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	binding, err := imgStore.Get(lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("imagestore.Get: %v", err)
	}
	return obj, imgStore, binding
}

// rebindManifestAnnotations rewrites lay's manifest with the given manifest and
// config-descriptor annotations, stores it in obj under its new digest, rebinds
// binding to that digest in imgStore, and returns the descriptor of the rewritten
// manifest -- the image's new identity.
func rebindManifestAnnotations(t *testing.T, obj *objectstore.Store, imgStore *imagestore.Store, lay octest.Layout, binding imagestore.Binding, manifestAnn, configAnn map[string]string) ocispec.Descriptor {
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
	if err := obj.WriteBlob(context.Background(), root, strings.NewReader(string(raw))); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if err := imgStore.Put(root, lay.Platform, binding); err != nil {
		t.Fatalf("imagestore.Put: %v", err)
	}
	return ocispec.Descriptor{Digest: root, Size: int64(len(raw))}
}

type (
	pullBindingDoc struct {
		Platform ocispec.Platform `json:"platform"`
		RootFS   struct {
			Descriptor struct {
				MediaType string        `json:"mediaType"`
				Digest    digest.Digest `json:"digest"`
				Size      int64         `json:"size"`
			} `json:"descriptor"`
			Fsverity struct {
				Mode   string        `json:"mode"`
				Digest digest.Digest `json:"digest"`
				Source string        `json:"source"`
			} `json:"fsverity"`
		} `json:"rootfs"`
	}

	// pullRefDoc is the ref record as it is written to disk: resolved is the
	// document the reference landed on, identity the platform manifest the
	// image is keyed by.
	pullRefDoc struct {
		Ref       string         `json:"ref"`
		Resolved  pullRefDescDoc `json:"resolved"`
		Identity  pullRefDescDoc `json:"identity"`
		PulledAt  string         `json:"pulledAt"`
		Platforms []string       `json:"platforms"`
	}

	pullRefDescDoc struct {
		MediaType string        `json:"mediaType"`
		Digest    digest.Digest `json:"digest"`
		Size      int64         `json:"size"`
	}
)

func useRemoteDefaultTransport(t *testing.T, tr http.RoundTripper) {
	t.Helper()
	old := remote.DefaultTransport
	remote.DefaultTransport = tr
	t.Cleanup(func() {
		remote.DefaultTransport = old
	})
}

func registryHostImage(t *testing.T, layers int64) (ggcrv1.Image, []digest.Digest) {
	t.Helper()
	img, err := random.Image(1024, layers)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	cfg.OS = runtime.GOOS
	cfg.Architecture = runtime.GOARCH
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("set platform: %v", err)
	}

	diffIDs := make([]digest.Digest, 0, len(cfg.RootFS.DiffIDs))
	for _, id := range cfg.RootFS.DiffIDs {
		diffIDs = append(diffIDs, digest.Digest(id.String()))
	}
	return img, diffIDs
}

func pushRegistryImage(t *testing.T, host, repo string, img ggcrv1.Image, tr http.RoundTripper) digest.Digest {
	t.Helper()
	tag, err := name.NewTag(host + "/" + repo + ":latest")
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := remote.Write(tag, img, remote.WithTransport(tr)); err != nil {
		t.Fatalf("push image: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}
	return digest.Digest(d.String())
}

func assertDiffIDs(t *testing.T, got, want []digest.Digest) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("DiffIDs len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("DiffIDs[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// stripBindingSize rewrites an image binding without its rootfs.size field,
// reproducing a binding written before the field existed.
func stripBindingSize(t *testing.T, root string, d digest.Digest, p ocispec.Platform) {
	t.Helper()
	path := octest.BindingPath(root, d, p)
	data, err := os.ReadFile(path) // #nosec G304 -- test rewrites a path derived from the digest it wrote
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal binding: %v", err)
	}
	var rootfs map[string]json.RawMessage
	if err := json.Unmarshal(doc["rootfs"], &rootfs); err != nil {
		t.Fatalf("unmarshal binding rootfs: %v", err)
	}
	delete(rootfs, "size")
	rootfsRaw, err := json.Marshal(rootfs)
	if err != nil {
		t.Fatalf("marshal binding rootfs: %v", err)
	}
	doc["rootfs"] = rootfsRaw
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write binding: %v", err)
	}
}

func readPullBinding(t *testing.T, root string, d digest.Digest, p ocispec.Platform) pullBindingDoc {
	t.Helper()
	data, err := os.ReadFile(octest.BindingPath(root, d, p)) // #nosec G304 -- test reads a path derived from the digest it wrote
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	var got pullBindingDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal binding: %v", err)
	}
	return got
}

func readPullRef(t *testing.T, root, ref string) pullRefDoc {
	t.Helper()
	data, err := os.ReadFile(octest.RefPath(root, ref)) // #nosec G304 -- test reads a path derived from the ref it wrote
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	var got pullRefDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal ref: %v", err)
	}
	return got
}

func assertObjectExists(t *testing.T, root string, d digest.Digest) {
	t.Helper()
	if _, err := os.Stat(octest.ObjectPath(root, d)); err != nil {
		t.Fatalf("object %s not present: %v", d, err)
	}
}
