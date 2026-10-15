// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/ociseal"
	"github.com/foundriesio/ocicfs/internal/octest"
)

func TestSealRegistryAndReseal(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "usr/bin", Dir: true},
		octest.File{Name: "empty", Mode: 0o644},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: "#!/bin/sh\n"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	image := layoutImage(t, layout)
	inputTag, err := name.NewTag("registry.example.com/team/app:input")
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if err := remote.Write(inputTag, image, remote.WithTransport(registry.Transport())); err != nil {
		t.Fatalf("push input image: %v", err)
	}
	counter := &putCounter{inner: registry.Transport()}
	remote.DefaultTransport = counter

	ref := "registry.example.com/team/app@" + layout.Root.String()
	dryRun, err := Seal(context.Background(), ref, Options{DryRun: true, Tag: "dry-run"})
	if err != nil {
		t.Fatalf("Seal dry run: %v", err)
	}
	dryRef, err := name.NewDigest("registry.example.com/team/app@" + dryRun.Sealed.String())
	if err != nil {
		t.Fatalf("dry-run digest reference: %v", err)
	}
	if _, err := remote.Get(dryRef, remote.WithTransport(registry.Transport())); err == nil {
		t.Fatal("dry run pushed the sealed manifest")
	}
	if got := counter.puts.Load(); got != 0 {
		t.Fatalf("dry run issued %d PUT requests, want 0", got)
	}

	result, err := Seal(context.Background(), ref, Options{Tag: "sealed"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if result.Sealed != dryRun.Sealed {
		t.Fatalf("Seal digest = %s, dry run computed %s", result.Sealed, dryRun.Sealed)
	}
	if result.Sealed == result.Original {
		t.Fatal("Seal did not change root digest")
	}
	assertRemoteAnnotations(t, registry, result)
	originalSealed := fetchRawManifest(t, registry, "registry.example.com/team/app@"+result.Sealed.String())

	resealed, err := Seal(context.Background(), "registry.example.com/team/app@"+result.Sealed.String(), Options{Tag: "resealed"})
	if err != nil {
		t.Fatalf("Seal second pass: %v", err)
	}
	if resealed.Sealed != result.Sealed || resealed.Original != result.Sealed {
		t.Fatalf("resealed result = %#v, want root %s", resealed, result.Sealed)
	}
	resealedTag, err := name.NewTag("registry.example.com/team/app:resealed")
	if err != nil {
		t.Fatalf("resealed tag: %v", err)
	}
	taggedReseal, err := remote.Get(resealedTag, remote.WithTransport(registry.Transport()))
	if err != nil {
		t.Fatalf("get resealed tag: %v", err)
	}
	if got := digest.Digest(taggedReseal.Digest.String()); got != result.Sealed {
		t.Fatalf("resealed tag digest = %s, want %s", got, result.Sealed)
	}
	if !bytes.Equal(taggedReseal.Manifest, originalSealed) {
		t.Fatal("resealed tag did not preserve the unchanged root bytes")
	}

	tag, err := name.NewTag("registry.example.com/team/app:sealed")
	if err != nil {
		t.Fatalf("sealed tag: %v", err)
	}
	tagged, err := remote.Get(tag, remote.WithTransport(registry.Transport()))
	if err != nil {
		t.Fatalf("get sealed tag: %v", err)
	}
	if got := digest.Digest(tagged.Digest.String()); got != result.Sealed {
		t.Fatalf("sealed tag digest = %s, want %s", got, result.Sealed)
	}
}

// TestSealFromDir seals from a local OCI image layout: the dry run completes
// with no network access at all, and the push run computes the same sealed
// digest while touching no registry blob endpoint — only manifests move.
func TestSealFromDir(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "usr/bin", Dir: true},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: "#!/bin/sh\n"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	ref := "registry.example.com/team/app@" + layout.Root.String()

	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = failTransport{}
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})
	dryRun, err := Seal(context.Background(), ref, Options{FromDir: layout.Dir, DryRun: true})
	if err != nil {
		t.Fatalf("Seal --from-dir dry run: %v", err)
	}
	if dryRun.Sealed == layout.Root {
		t.Fatal("dry run did not change the root digest")
	}

	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	remote.DefaultTransport = registry.Transport()
	pushImage(t, registry, "registry.example.com/team/app:input", layoutImage(t, layout))

	blobs := &blobCounter{inner: registry.Transport()}
	remote.DefaultTransport = blobs
	result, err := Seal(context.Background(), ref, Options{FromDir: layout.Dir})
	if err != nil {
		t.Fatalf("Seal --from-dir: %v", err)
	}
	if result.Sealed != dryRun.Sealed {
		t.Fatalf("sealed digest = %s, offline dry run computed %s", result.Sealed, dryRun.Sealed)
	}
	if n := blobs.hits.Load(); n != 0 {
		t.Fatalf("seal touched registry blob endpoints %d times; --from-dir must read blobs from the layout", n)
	}
	remote.DefaultTransport = registry.Transport()
	assertRemoteAnnotations(t, registry, result)
}

// TestSealFromDirRejectsInsecureRegistry applies the registry HTTPS policy to
// the local-layout path too: a reference whose pushes would travel over plain
// HTTP to a non-loopback registry is rejected before anything is read.
func TestSealFromDirRejectsInsecureRegistry(t *testing.T) {
	ref := "10.0.0.5:5000/team/app@sha256:" + strings.Repeat("a", 64)
	_, err := Seal(context.Background(), ref, Options{FromDir: t.TempDir(), DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "does not use HTTPS") {
		t.Fatalf("err = %v, want an HTTPS policy rejection", err)
	}
}

// failTransport fails every request, so a test can assert an operation is
// fully offline.
type failTransport struct{}

func (failTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected network request: %s %s", req.Method, req.URL)
}

// blobCounter tallies requests to registry blob endpoints (reads and upload
// starts alike), so a test can assert every blob byte came from elsewhere.
type blobCounter struct {
	inner http.RoundTripper
	hits  atomic.Int64
}

func (c *blobCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/blobs/") {
		c.hits.Add(1)
	}
	return c.inner.RoundTrip(req)
}

func TestSealIndexPreservesUnknownFieldsAndAttestation(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	amdPlatform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	armPlatform := ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	amdFixture := octest.BuildIndexImage(t, amdPlatform, "amd64")
	armFixture := octest.BuildIndexImage(t, armPlatform, "arm64")
	amdImage := layoutImage(t, amdFixture)
	armImage := layoutImage(t, armFixture)
	pushImage(t, registry, "registry.example.com/team/multi:amd64", amdImage)
	pushImage(t, registry, "registry.example.com/team/multi:arm64", armImage)

	amdDesc := imageDescriptor(t, amdImage, amdPlatform)
	armDesc := imageDescriptor(t, armImage, armPlatform)
	repo, err := name.NewRepository("registry.example.com/team/multi")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	// The shape Docker Hub publishes: an attestation manifest carries a regular
	// image config and is distinguishable from an image only by its layer types.
	attestationDesc := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"config": map[string]any{
			"mediaType": ocispec.MediaTypeImageConfig,
			"digest":    "sha256:" + strings.Repeat("c", 64),
			"size":      json.Number("233"),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.in-toto+json",
			"digest":    "sha256:" + strings.Repeat("d", 64),
			"size":      json.Number("1024"),
		}},
		"annotations": map[string]any{
			"vnd.docker.reference.type": "attestation-manifest",
		},
	}, ocispec.MediaTypeImageManifest)
	indexDoc := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"annotations":   map[string]any{"vendor.index": "kept"},
		"x-vendor-index": map[string]any{
			"number": json.Number("9007199254740993"),
		},
		"manifests": []any{
			descriptorDocument(amdDesc, map[string]any{"x-vendor-descriptor": "amd"}),
			descriptorDocument(armDesc, map[string]any{"x-vendor-descriptor": "arm"}),
			map[string]any{
				"mediaType": ocispec.MediaTypeImageManifest,
				"digest":    attestationDesc.Digest.String(),
				"size":      json.Number(fmt.Sprint(attestationDesc.Size)),
				"platform":  map[string]any{"os": "unknown", "architecture": "unknown"},
				"annotations": map[string]any{
					"vnd.docker.reference.type": "attestation-manifest",
					"vendor.attestation":        "kept",
				},
				"x-attestation": map[string]any{"kept": true},
			},
		},
	}
	rawIndex, err := json.Marshal(indexDoc)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	root := digest.FromBytes(rawIndex)
	if err := putManifest(context.Background(), repo.Digest(root.String()), rawIndex, ocispec.MediaTypeImageIndex); err != nil {
		t.Fatalf("push index: %v", err)
	}
	puts := &putRecorder{inner: registry.Transport()}
	remote.DefaultTransport = puts

	// One platform per call: seal amd64, then arm64 on the digest the first
	// call produced. Each intermediate index is a valid, partially sealed
	// state.
	first, err := Seal(context.Background(), "registry.example.com/team/multi@"+root.String(), Options{Platform: "linux/amd64", Tag: "amd64-sealed"})
	if err != nil {
		t.Fatalf("Seal amd64: %v", err)
	}
	if len(first.Platforms) != 1 || first.Platforms[0].Platform.Architecture != "amd64" {
		t.Fatalf("first result platforms = %#v, want one amd64 entry", first.Platforms)
	}
	wantPuts := []string{
		"/v2/team/multi/manifests/" + first.Platforms[0].Sealed.String(),
		"/v2/team/multi/manifests/" + first.Sealed.String(),
		"/v2/team/multi/manifests/amd64-sealed",
	}
	if got := puts.paths(); !equalStrings(got, wantPuts) {
		t.Fatalf("PUT order = %v, want %v", got, wantPuts)
	}
	remote.DefaultTransport = registry.Transport()
	originalDescriptors := indexDoc["manifests"].([]any)
	firstRaw := fetchRawManifest(t, registry, "registry.example.com/team/multi@"+first.Sealed.String())
	firstDoc, err := decodeDocument(firstRaw)
	if err != nil {
		t.Fatalf("decode first sealed index: %v", err)
	}
	firstDescriptors := firstDoc["manifests"].([]any)
	if firstDescriptors[0].(map[string]any)["digest"] == originalDescriptors[0].(map[string]any)["digest"] {
		t.Fatal("amd64 descriptor was not rewritten")
	}
	if !documentsEqual(t, firstDescriptors[1], originalDescriptors[1]) {
		t.Fatal("unsealed arm64 descriptor changed in the amd64 call")
	}
	if !documentsEqual(t, firstDescriptors[2], originalDescriptors[2]) {
		t.Fatal("attestation descriptor changed in the amd64 call")
	}

	result, err := Seal(context.Background(), "registry.example.com/team/multi@"+first.Sealed.String(), Options{Platform: "linux/arm64/v8"})
	if err != nil {
		t.Fatalf("Seal arm64: %v", err)
	}
	sealedRaw := fetchRawManifest(t, registry, "registry.example.com/team/multi@"+result.Sealed.String())
	sealedDoc, err := decodeDocument(sealedRaw)
	if err != nil {
		t.Fatalf("decode sealed index: %v", err)
	}
	if sealedDoc["annotations"].(map[string]any)["vendor.index"] != "kept" {
		t.Fatal("index annotation was not preserved")
	}
	if got := sealedDoc["x-vendor-index"].(map[string]any)["number"].(json.Number).String(); got != "9007199254740993" {
		t.Fatalf("vendor number = %s", got)
	}
	sealedDescriptors := sealedDoc["manifests"].([]any)
	for i := 0; i < 2; i++ {
		got := sealedDescriptors[i].(map[string]any)
		if got["digest"] == originalDescriptors[i].(map[string]any)["digest"] {
			t.Fatalf("platform descriptor %d digest was not rewritten", i)
		}
		if got["x-vendor-descriptor"] != originalDescriptors[i].(map[string]any)["x-vendor-descriptor"] {
			t.Fatalf("platform descriptor %d vendor field changed", i)
		}
	}
	if !documentsEqual(t, sealedDescriptors[2], originalDescriptors[2]) {
		t.Fatal("attestation descriptor changed")
	}
}

// TestSealFallbackAllowed covers --allow-non-canonical: a non-canonical
// construction is sealed with its deterministic fallback digest under the
// ocicfs fallback annotations — never the standard namespace — resealing it
// is a no-op, and a non-canonical manifest already wearing a standard merged
// annotation is refused even with the permission, since pull rejects that
// combination and sealing must not launder it.
func TestSealFallbackAllowed(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout := octest.NonCanonicalLayout(t, platform)
	image := layoutImage(t, layout)
	pushImage(t, registry, "registry.example.com/team/fallback:input", image)
	ref := "registry.example.com/team/fallback@" + layout.Root.String()

	result, err := Seal(context.Background(), ref, Options{AllowNonCanonical: true})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if result.Sealed == result.Original {
		t.Fatal("fallback seal did not change the manifest")
	}
	raw := fetchRawManifest(t, registry, "registry.example.com/team/fallback@"+result.Sealed.String())
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse sealed manifest: %v", err)
	}
	if got := "sha256:" + manifest.Annotations[ociseal.FallbackMergedAnnotation]; got != result.Platforms[0].MergedFsverity {
		t.Fatalf("fallback merged annotation = %q, want %q", got, result.Platforms[0].MergedFsverity)
	}
	if _, err := ociseal.ParseAnnotation(manifest.Config.Annotations[ociseal.FallbackConfigAnnotation]); err != nil {
		t.Fatalf("fallback config annotation: %v", err)
	}
	for key := range manifest.Annotations {
		if strings.HasPrefix(key, ociseal.MergedAnnotationPrefix) {
			t.Fatalf("fallback seal wrote standard-namespace annotation %s", key)
		}
	}
	if _, ok := manifest.Config.Annotations[ociseal.ConfigAnnotation]; ok {
		t.Fatal("fallback seal wrote the standard config annotation")
	}

	resealed, err := Seal(context.Background(), "registry.example.com/team/fallback@"+result.Sealed.String(), Options{AllowNonCanonical: true})
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if resealed.Sealed != result.Sealed {
		t.Fatalf("reseal changed digest: %s -> %s", result.Sealed, resealed.Sealed)
	}
}

func TestSealFallbackRefusesStandardAnnotation(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout := octest.NonCanonicalLayout(t, platform)
	image := layoutImage(t, layout)
	pushImage(t, registry, "registry.example.com/team/launder:input", image)

	repo, err := name.NewRepository("registry.example.com/team/launder")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	doc, err := decodeDocument(fetchRawManifest(t, registry, "registry.example.com/team/launder@"+layout.Root.String()))
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	doc["annotations"] = map[string]any{ociseal.MergedAnnotation: strings.Repeat("a", 64)}
	tainted := pushDocument(t, repo, doc, ocispec.MediaTypeImageManifest)

	_, err = Seal(context.Background(), "registry.example.com/team/launder@"+tainted.Digest.String(), Options{AllowNonCanonical: true})
	if err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("Seal err = %v, want standard-annotation refusal", err)
	}
}

func TestSealMixedIndexDeclinesAndPushesNothing(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	counter := &putCounter{inner: registry.Transport()}
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = counter
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	amdPlatform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	armPlatform := ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	amdFixture := octest.BuildIndexImage(t, amdPlatform, "amd64")
	armFixture := octest.NonCanonicalLayout(t, armPlatform)
	amdImage := layoutImage(t, amdFixture)
	armImage := layoutImage(t, armFixture)
	pushImage(t, registry, "registry.example.com/team/mixed:amd64", amdImage)
	pushImage(t, registry, "registry.example.com/team/mixed:arm64", armImage)

	repo, err := name.NewRepository("registry.example.com/team/mixed")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	amdDesc := imageDescriptor(t, amdImage, amdPlatform)
	armDesc := imageDescriptor(t, armImage, armPlatform)
	indexDoc := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests": []any{
			descriptorDocument(amdDesc, nil),
			descriptorDocument(armDesc, nil),
		},
	}
	root := pushDocument(t, repo, indexDoc, ocispec.MediaTypeImageIndex)

	// The non-canonical platform declines without writing anything; the
	// canonical sibling seals fine in its own call — a decline is per
	// platform, not per index.
	counter.puts.Store(0)
	_, err = Seal(context.Background(), "registry.example.com/team/mixed@"+root.Digest.String(), Options{Platform: "linux/arm64/v8", Tag: "sealed"})
	if !errors.Is(err, ociseal.ErrNotCanonical) {
		t.Fatalf("Seal err = %v, want ErrNotCanonical", err)
	}
	if got := counter.puts.Load(); got != 0 {
		t.Fatalf("declined seal issued %d PUT requests, want 0", got)
	}
	tag, err := name.NewTag("registry.example.com/team/mixed:sealed")
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if _, err := remote.Get(tag, remote.WithTransport(registry.Transport())); err == nil {
		t.Fatal("declined seal tagged the image")
	}

	sealed, err := Seal(context.Background(), "registry.example.com/team/mixed@"+root.Digest.String(), Options{Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("Seal canonical sibling: %v", err)
	}
	if sealed.Sealed == root.Digest {
		t.Fatal("sealing the canonical platform did not change the index")
	}
}

func TestSealNestedIndexDeclines(t *testing.T) {
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	counter := &putCounter{inner: registry.Transport()}
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = counter
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	repo, err := name.NewRepository("registry.example.com/team/nested")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	inner := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests":     []any{},
	}, ocispec.MediaTypeImageIndex)
	innerDesc := rawDescriptor(inner)
	innerDesc["platform"] = map[string]any{"os": "linux", "architecture": "amd64"}
	outer := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests":     []any{innerDesc},
	}, ocispec.MediaTypeImageIndex)

	counter.puts.Store(0)
	_, err = Seal(context.Background(), "registry.example.com/team/nested@"+outer.Digest.String(), Options{Platform: "linux/amd64"})
	if err == nil || !strings.Contains(err.Error(), "nested index") {
		t.Fatalf("Seal err = %v, want nested index rejection", err)
	}
	if got := counter.puts.Load(); got != 0 {
		t.Fatalf("nested-index seal issued %d PUT requests, want 0", got)
	}
}

func TestSealDirectAttestationDeclines(t *testing.T) {
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	repo, err := name.NewRepository("registry.example.com/team/att")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	attDesc := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"config": map[string]any{
			"mediaType": ocispec.MediaTypeImageConfig,
			"digest":    "sha256:" + strings.Repeat("c", 64),
			"size":      json.Number("233"),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.in-toto+json",
			"digest":    "sha256:" + strings.Repeat("d", 64),
			"size":      json.Number("1024"),
		}},
	}, ocispec.MediaTypeImageManifest)
	_, err = Seal(context.Background(), "registry.example.com/team/att@"+attDesc.Digest.String(), Options{})
	if err == nil || !strings.Contains(err.Error(), "not a container image manifest") {
		t.Fatalf("Seal error = %v, want a not-an-image decline", err)
	}
}

func TestSealTouchesOnlySelectedChild(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	amdPlatform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	amdFixture := octest.BuildIndexImage(t, amdPlatform, "amd64")
	amdImage := layoutImage(t, amdFixture)
	pushImage(t, registry, "registry.example.com/team/liar:amd64", amdImage)

	repo, err := name.NewRepository("registry.example.com/team/liar")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	amdDesc := imageDescriptor(t, amdImage, amdPlatform)
	// A real sibling manifest that sealing amd64 must never fetch: the seal
	// touches exactly the selected child, so the sibling's manifest endpoint
	// must see zero requests.
	sibling := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"config": map[string]any{
			"mediaType": ocispec.MediaTypeImageConfig,
			"digest":    "sha256:" + strings.Repeat("c", 64),
			"size":      json.Number("233"),
		},
		"layers": []any{},
	}, ocispec.MediaTypeImageManifest)
	siblingDesc := rawDescriptor(sibling)
	siblingDesc["platform"] = map[string]any{"os": "linux", "architecture": "arm64"}
	indexDoc := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests": []any{
			descriptorDocument(amdDesc, nil),
			siblingDesc,
		},
	}
	root := pushDocument(t, repo, indexDoc, ocispec.MediaTypeImageIndex)

	fetches := &pathCounter{inner: registry.Transport(), path: "/manifests/" + sibling.Digest.String()}
	remote.DefaultTransport = fetches

	sealed, err := Seal(context.Background(), "registry.example.com/team/liar@"+root.Digest.String(), Options{Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := fetches.hits.Load(); got != 0 {
		t.Fatalf("sealing amd64 touched the arm64 sibling %d times, want 0", got)
	}
	sealedDoc, err := decodeDocument(fetchRawManifest(t, registry, "registry.example.com/team/liar@"+sealed.Sealed.String()))
	if err != nil {
		t.Fatalf("decode sealed index: %v", err)
	}
	if !documentsEqual(t, sealedDoc["manifests"].([]any)[1], siblingDesc) {
		t.Fatal("untouched sibling descriptor changed")
	}
}

// pathCounter tallies requests whose URL path ends with the given suffix, so a
// test can assert an endpoint was never touched.
type pathCounter struct {
	inner http.RoundTripper
	path  string
	hits  atomic.Int64
}

func (c *pathCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, c.path) {
		c.hits.Add(1)
	}
	return c.inner.RoundTrip(req)
}

func TestSealPlatformSelectionErrors(t *testing.T) {
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	repo, err := name.NewRepository("registry.example.com/team/select")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	manifest := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"config": map[string]any{
			"mediaType": ocispec.MediaTypeImageConfig,
			"digest":    "sha256:" + strings.Repeat("c", 64),
			"size":      json.Number("233"),
		},
		"layers": []any{},
	}, ocispec.MediaTypeImageManifest)
	// Selection errors fire before the chosen child is fetched; the children
	// just have to exist for the index push to be accepted.
	child := func(arch string) map[string]any {
		desc := rawDescriptor(manifest)
		desc["platform"] = map[string]any{"os": "linux", "architecture": arch}
		return desc
	}
	index := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests":     []any{child("amd64"), child("arm64"), child("arm64")},
	}, ocispec.MediaTypeImageIndex)

	tests := []struct {
		name     string
		target   ocispec.Descriptor
		platform string
		want     string
	}{
		{"index without platform", index, "", "select one platform"},
		{"platform not in index", index, "linux/riscv64", "no manifest for platform"},
		{"ambiguous platform", index, "linux/arm64", "matches more than one"},
		{"manifest with platform", manifest, "linux/amd64", "applies only to an index"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Seal(context.Background(),
				"registry.example.com/team/select@"+tt.target.Digest.String(),
				Options{Platform: tt.platform})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Seal err = %v, want %q", err, tt.want)
			}
		})
	}
}

// pushDocument marshals doc, publishes it under its own digest through the
// manifests endpoint, and returns a descriptor pinning it.
func pushDocument(t *testing.T, repo name.Repository, doc map[string]any, mediaType string) ocispec.Descriptor {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	d := digest.FromBytes(raw)
	if err := putManifest(context.Background(), repo.Digest(d.String()), raw, mediaType); err != nil {
		t.Fatalf("push document: %v", err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(raw))}
}

// rawDescriptor renders desc as the minimal JSON descriptor map an index entry
// carries.
func rawDescriptor(desc ocispec.Descriptor) map[string]any {
	return map[string]any{
		"mediaType": desc.MediaType,
		"digest":    desc.Digest.String(),
		"size":      json.Number(fmt.Sprint(desc.Size)),
	}
}

// putCounter tallies the PUT requests reaching the wrapped transport, so a test
// can assert a declined seal writes nothing.
type putCounter struct {
	inner http.RoundTripper
	puts  atomic.Int64
}

func (c *putCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut {
		c.puts.Add(1)
	}
	return c.inner.RoundTrip(req)
}

type putRecorder struct {
	inner http.RoundTripper
	mu    sync.Mutex
	puts  []string
}

func (r *putRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut {
		r.mu.Lock()
		r.puts = append(r.puts, req.URL.Path)
		r.mu.Unlock()
	}
	return r.inner.RoundTrip(req)
}

func (r *putRecorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.puts...)
}

func layoutImage(t *testing.T, fixture octest.Layout) ggcrv1.Image {
	t.Helper()
	path, err := ggcrlayout.FromPath(fixture.Dir)
	if err != nil {
		t.Fatalf("layout.FromPath: %v", err)
	}
	hash, err := ggcrv1.NewHash(fixture.Root.String())
	if err != nil {
		t.Fatalf("v1.NewHash: %v", err)
	}
	image, err := path.Image(hash)
	if err != nil {
		t.Fatalf("layout.Image: %v", err)
	}
	return image
}

func pushImage(t *testing.T, registry *octest.Registry, ref string, image ggcrv1.Image) {
	t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if err := remote.Write(tag, image, remote.WithTransport(registry.Transport())); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
}

func imageDescriptor(t *testing.T, image ggcrv1.Image, platform ocispec.Platform) ocispec.Descriptor {
	t.Helper()
	hash, err := image.Digest()
	if err != nil {
		t.Fatalf("image.Digest: %v", err)
	}
	raw, err := image.RawManifest()
	if err != nil {
		t.Fatalf("image.RawManifest: %v", err)
	}
	mediaType, err := image.MediaType()
	if err != nil {
		t.Fatalf("image.MediaType: %v", err)
	}
	return ocispec.Descriptor{
		MediaType: string(mediaType),
		Digest:    digest.Digest(hash.String()),
		Size:      int64(len(raw)),
		Platform:  &platform,
	}
}

func descriptorDocument(desc ocispec.Descriptor, extra map[string]any) map[string]any {
	doc := map[string]any{
		"mediaType": desc.MediaType,
		"digest":    desc.Digest.String(),
		"size":      json.Number(fmt.Sprint(desc.Size)),
		"platform": map[string]any{
			"os":           desc.Platform.OS,
			"architecture": desc.Platform.Architecture,
		},
	}
	if desc.Platform.Variant != "" {
		doc["platform"].(map[string]any)["variant"] = desc.Platform.Variant
	}
	for key, value := range extra {
		doc[key] = value
	}
	return doc
}

func fetchRawManifest(t *testing.T, registry *octest.Registry, ref string) []byte {
	t.Helper()
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		t.Fatalf("name.NewDigest: %v", err)
	}
	desc, err := remote.Get(digestRef, remote.WithTransport(registry.Transport()))
	if err != nil {
		t.Fatalf("remote.Get: %v", err)
	}
	return desc.Manifest
}

func documentsEqual(t *testing.T, left, right any) bool {
	t.Helper()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatalf("marshal left document: %v", err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatalf("marshal right document: %v", err)
	}
	return bytes.Equal(leftJSON, rightJSON)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func decodeDocument(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON document")
	}
	return doc, nil
}

func assertRemoteAnnotations(t *testing.T, registry *octest.Registry, result ociseal.Result) {
	t.Helper()
	ref, err := name.NewDigest("registry.example.com/team/app@" + result.Sealed.String())
	if err != nil {
		t.Fatalf("name.NewDigest: %v", err)
	}
	desc, err := remote.Get(ref, remote.WithTransport(registry.Transport()))
	if err != nil {
		t.Fatalf("get sealed manifest: %v", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(desc.Manifest, &manifest); err != nil {
		t.Fatalf("parse sealed manifest: %v", err)
	}
	if got := "sha256:" + manifest.Annotations[ociseal.MergedAnnotation]; got != result.Platforms[0].MergedFsverity {
		t.Fatalf("merged annotation = %q, want %q", got, result.Platforms[0].MergedFsverity)
	}
	if _, err := ociseal.ParseAnnotation(manifest.Config.Annotations[ociseal.ConfigAnnotation]); err != nil {
		t.Fatalf("config annotation: %v", err)
	}
}
