// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/octest"
)

func TestParseAnnotation(t *testing.T) {
	hexDigest := strings.Repeat("a", 64)
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "valid", value: hexDigest, want: "sha256:" + hexDigest},
		{name: "prefixed", value: "sha256:" + hexDigest, wantErr: true},
		{name: "uppercase", value: strings.ToUpper(hexDigest), wantErr: true},
		{name: "short", value: hexDigest[:63], wantErr: true},
		{name: "non-hex", value: strings.Repeat("z", 64), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAnnotation(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseAnnotation succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAnnotation: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseAnnotation = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyAnnotation(t *testing.T) {
	hexDigest := strings.Repeat("a", 64)
	actual := "sha256:" + hexDigest
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "match", value: hexDigest},
		{name: "mismatch", value: strings.Repeat("b", 64), wantErr: true},
		{name: "prefixed", value: actual, wantErr: true},
		{name: "uppercase", value: strings.ToUpper(hexDigest), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyAnnotation(MergedAnnotation, tt.value, actual)
			if (err != nil) != tt.wantErr {
				t.Fatalf("verifyAnnotation error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestMergedAnnotationSharesPrefix(t *testing.T) {
	if !strings.HasPrefix(MergedAnnotation, MergedAnnotationPrefix) {
		t.Fatalf("%q does not share prefix %q", MergedAnnotation, MergedAnnotationPrefix)
	}
}

// TestVerifyAnnotationsNonCanonical exercises the rules for a non-canonical
// construction: any annotation under the merged-EROFS namespace is rejected
// regardless of suffix or value, a present knarr fallback annotation must
// match the measured fallback digest, and an image carrying neither is a
// valid unsealed pull. These paths never measure a stored blob, so a plain
// store on any filesystem suffices.
func TestVerifyAnnotationsNonCanonical(t *testing.T) {
	store, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	hexDigest := strings.Repeat("a", 64)
	tests := []struct {
		name        string
		annotations map[string]string
		measured    string
		wantErr     bool
	}{
		{name: "exact merged key", annotations: map[string]string{MergedAnnotation: hexDigest}, wantErr: true},
		{name: "sha512 suffix", annotations: map[string]string{"composefs.merged.erofs.v1.fsverity-sha512-12": hexDigest}, wantErr: true},
		{name: "signature suffix", annotations: map[string]string{"composefs.merged.erofs.v1.fsverity-sha256-12.sig": "not-a-digest"}, wantErr: true},
		{name: "config namespace only", annotations: map[string]string{ConfigAnnotation: hexDigest}, wantErr: false},
		{name: "unrelated annotation", annotations: map[string]string{"org.opencontainers.image.title": "app"}, wantErr: false},
		{name: "no annotations", annotations: nil, wantErr: false},
		{name: "fallback match", annotations: map[string]string{FallbackMergedAnnotation: hexDigest}, measured: "sha256:" + hexDigest, wantErr: false},
		{name: "fallback mismatch", annotations: map[string]string{FallbackMergedAnnotation: hexDigest}, measured: "sha256:" + strings.Repeat("b", 64), wantErr: true},
		{name: "fallback beside merged key", annotations: map[string]string{FallbackMergedAnnotation: hexDigest, MergedAnnotation: hexDigest}, measured: "sha256:" + hexDigest, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ocichain.Resolved{Manifest: ocispec.Manifest{Annotations: tt.annotations}}
			err := VerifyAnnotations(context.Background(), store, res, tt.measured, false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("VerifyAnnotations error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestAnnotateManifestPreservesUnknownFieldsAndIsIdempotent(t *testing.T) {
	merged := "sha256:" + strings.Repeat("a", 64)
	config := "sha256:" + strings.Repeat("b", 64)
	raw := []byte(`{
	  "schemaVersion": 2,
	  "mediaType": "application/vnd.oci.image.manifest.v1+json",
	  "x-vendor-number": 9007199254740993,
	  "config": {
	    "mediaType": "application/vnd.oci.image.config.v1+json",
	    "digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	    "size": 10,
	    "x-vendor-config": {"kept": true},
	    "annotations": {"vendor.config": "kept"}
	  },
	  "layers": [],
	  "annotations": {"vendor.manifest": "kept"}
	}`)

	sealed, changed, err := annotateManifest(raw, merged, config, true)
	if err != nil {
		t.Fatalf("annotateManifest: %v", err)
	}
	if !changed {
		t.Fatal("annotateManifest reported unchanged")
	}
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(sealed))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode sealed manifest: %v", err)
	}
	if got := doc["x-vendor-number"].(json.Number).String(); got != "9007199254740993" {
		t.Fatalf("vendor number = %s", got)
	}
	configDoc := doc["config"].(map[string]any)
	if configDoc["x-vendor-config"].(map[string]any)["kept"] != true {
		t.Fatal("vendor config field was not preserved")
	}

	resealed, changed, err := annotateManifest(sealed, merged, config, true)
	if err != nil {
		t.Fatalf("annotateManifest second pass: %v", err)
	}
	if changed {
		t.Fatal("second pass reported changed")
	}
	if !bytes.Equal(resealed, sealed) {
		t.Fatal("idempotent pass changed original raw bytes")
	}
}

func TestAnnotateManifestRejectsMismatch(t *testing.T) {
	raw := []byte(`{
	  "schemaVersion": 2,
	  "config": {"annotations": {
	    "composefs.config.fsverity-sha256-12": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	  }},
	  "annotations": {
	    "composefs.merged.erofs.v1.fsverity-sha256-12": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	  }
	}`)
	_, _, err := annotateManifest(raw,
		"sha256:"+strings.Repeat("c", 64),
		"sha256:"+strings.Repeat("b", 64),
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("annotateManifest error = %v, want mismatch", err)
	}
}

func TestSealBareManifestDryRun(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "etc", Dir: true},
		octest.File{Name: "empty", Mode: 0o644},
		octest.File{Name: "etc/value", Mode: 0o644, Body: "value"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	src, err := ocilayout.Open(layout.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	repo, err := name.NewRepository("registry.example.com/team/app")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	result, err := seal(context.Background(), src, repo, layout.Root, nil, nil, true, false)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if result.Original != layout.Root || result.Sealed == layout.Root {
		t.Fatalf("result = %#v, want changed root %s", result, layout.Root)
	}
	if len(result.Platforms) != 1 ||
		result.Platforms[0].Platform.OS != platform.OS ||
		result.Platforms[0].Platform.Architecture != platform.Architecture {
		t.Fatalf("platforms = %#v, want %#v", result.Platforms, platform)
	}
	if err := digest.Digest(result.Platforms[0].MergedFsverity).Validate(); err != nil {
		t.Fatalf("merged fs-verity digest = %q: %v", result.Platforms[0].MergedFsverity, err)
	}
}

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

	ref := "registry.example.com/team/app@" + layout.Root.String()
	dryRun, err := Seal(context.Background(), ref, Options{DryRun: true})
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

func TestSealGoldenDigest(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "etc", Dir: true},
		octest.File{Name: "empty", Mode: 0o644},
		octest.File{Name: "etc/golden", Mode: 0o644, Body: "fixed synthetic input\n"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	src, err := ocilayout.Open(layout.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	repo, err := name.NewRepository("registry.example.com/team/golden")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	result, err := seal(context.Background(), src, repo, layout.Root, nil, nil, true, false)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	const want = "sha256:9238176e13729b9215caf137866624e2f0d053c6e440e5446c824799d1d00123"
	if result.Platforms[0].MergedFsverity != want {
		t.Fatalf("merged fs-verity = %s, want %s", result.Platforms[0].MergedFsverity, want)
	}
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
	amdFixture := buildIndexImage(t, amdPlatform, "amd64")
	armFixture := buildIndexImage(t, armPlatform, "arm64")
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

	// One platform per call: seal amd64, then arm64 on the digest the first
	// call produced. Each intermediate index is a valid, partially sealed
	// state.
	first, err := Seal(context.Background(), "registry.example.com/team/multi@"+root.String(), Options{Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("Seal amd64: %v", err)
	}
	if len(first.Platforms) != 1 || first.Platforms[0].Platform.Architecture != "amd64" {
		t.Fatalf("first result platforms = %#v, want one amd64 entry", first.Platforms)
	}
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

func TestSealDeclinesNonCanonical(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout := nonCanonicalLayout(t, platform)
	src, err := ocilayout.Open(layout.Dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	repo, err := name.NewRepository("registry.example.com/team/bad")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	for _, dryRun := range []bool{false, true} {
		_, err := seal(context.Background(), src, repo, layout.Root, nil, nil, dryRun, false)
		if !errors.Is(err, ErrNotCanonical) {
			t.Fatalf("dryRun=%v: err = %v, want ErrNotCanonical", dryRun, err)
		}
	}
}

// TestSealFallbackAllowed covers --allow-non-canonical: a non-canonical
// construction is sealed with its deterministic fallback digest under the
// knarr fallback annotations — never the standard namespace — resealing it
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
	layout := nonCanonicalLayout(t, platform)
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
	if got := "sha256:" + manifest.Annotations[FallbackMergedAnnotation]; got != result.Platforms[0].MergedFsverity {
		t.Fatalf("fallback merged annotation = %q, want %q", got, result.Platforms[0].MergedFsverity)
	}
	if _, err := ParseAnnotation(manifest.Config.Annotations[FallbackConfigAnnotation]); err != nil {
		t.Fatalf("fallback config annotation: %v", err)
	}
	for key := range manifest.Annotations {
		if strings.HasPrefix(key, MergedAnnotationPrefix) {
			t.Fatalf("fallback seal wrote standard-namespace annotation %s", key)
		}
	}
	if _, ok := manifest.Config.Annotations[ConfigAnnotation]; ok {
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
	layout := nonCanonicalLayout(t, platform)
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
	doc["annotations"] = map[string]any{MergedAnnotation: strings.Repeat("a", 64)}
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
	amdFixture := buildIndexImage(t, amdPlatform, "amd64")
	armFixture := nonCanonicalLayout(t, armPlatform)
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
	if !errors.Is(err, ErrNotCanonical) {
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

func TestIsImageManifest(t *testing.T) {
	config := `{"mediaType":"` + ocispec.MediaTypeImageConfig + `","digest":"sha256:` + strings.Repeat("a", 64) + `","size":1}`
	tarLayer := `{"mediaType":"` + ocispec.MediaTypeImageLayerGzip + `"}`
	dockerLayer := `{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip"}`
	inTotoLayer := `{"mediaType":"application/vnd.in-toto+json"}`
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{"oci gzip layers", `{"config":` + config + `,"layers":[` + tarLayer + `]}`, true},
		{"docker gzip layers", `{"config":` + config + `,"layers":[` + dockerLayer + `]}`, true},
		{"empty layers", `{"config":` + config + `,"layers":[]}`, true},
		{"no config", `{"layers":[` + tarLayer + `]}`, false},
		{"config without digest", `{"config":{"mediaType":"` + ocispec.MediaTypeImageConfig + `"},"layers":[` + tarLayer + `]}`, false},
		{"no layers key", `{"config":` + config + `}`, false},
		{"attestation with config and in-toto layer", `{"config":` + config + `,"layers":[` + inTotoLayer + `]}`, false},
		{"one non-tar layer among tar layers", `{"config":` + config + `,"layers":[` + tarLayer + `,` + inTotoLayer + `]}`, false},
		{"layer without media type", `{"config":` + config + `,"layers":[{}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var manifest ocispec.Manifest
			if err := json.Unmarshal([]byte(tt.doc), &manifest); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := isImageManifest(manifest); got != tt.want {
				t.Fatalf("isImageManifest = %v, want %v", got, tt.want)
			}
		})
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
	amdFixture := buildIndexImage(t, amdPlatform, "amd64")
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

// nonCanonicalLayout builds a single-layer image whose tree omits /usr, so the
// canonical construction substitutes default root metadata — a divergence that
// makes the image ineligible for sealing.
func nonCanonicalLayout(t *testing.T, platform ocispec.Platform) octest.Layout {
	t.Helper()
	layout, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "etc/value", Mode: 0o644, Body: "value"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	return layout
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

func buildIndexImage(t *testing.T, platform ocispec.Platform, body string) octest.Layout {
	t.Helper()
	fixture, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "etc", Dir: true},
		octest.File{Name: "empty", Mode: 0o644},
		octest.File{Name: "etc/platform", Mode: 0o644, Body: body},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	return fixture
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

func assertRemoteAnnotations(t *testing.T, registry *octest.Registry, result Result) {
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
	if got := "sha256:" + manifest.Annotations[MergedAnnotation]; got != result.Platforms[0].MergedFsverity {
		t.Fatalf("merged annotation = %q, want %q", got, result.Platforms[0].MergedFsverity)
	}
	if _, err := ParseAnnotation(manifest.Config.Annotations[ConfigAnnotation]); err != nil {
		t.Fatalf("config annotation: %v", err)
	}
}
