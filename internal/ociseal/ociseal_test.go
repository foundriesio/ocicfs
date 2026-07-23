// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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

	sealed, changed, err := annotateManifest(raw, merged, config)
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

	resealed, changed, err := annotateManifest(sealed, merged, config)
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
	result, err := seal(context.Background(), src, repo, layout.Root, nil, true)
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
	result, err := seal(context.Background(), src, repo, layout.Root, nil, true)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	const want = "sha256:9238176e13729b9215caf137866624e2f0d053c6e440e5446c824799d1d00123"
	if result.Platforms[0].MergedFsverity != want {
		t.Fatalf("merged fs-verity = %s, want %s", result.Platforms[0].MergedFsverity, want)
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
		_, err := seal(context.Background(), src, repo, layout.Root, nil, dryRun)
		if !errors.Is(err, ErrNotCanonical) {
			t.Fatalf("dryRun=%v: err = %v, want ErrNotCanonical", dryRun, err)
		}
	}
}

func TestSealIndexDeclines(t *testing.T) {
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	oldTransport := remote.DefaultTransport
	remote.DefaultTransport = registry.Transport()
	t.Cleanup(func() {
		remote.DefaultTransport = oldTransport
	})

	repo, err := name.NewRepository("registry.example.com/team/index")
	if err != nil {
		t.Fatalf("name.NewRepository: %v", err)
	}
	index := pushDocument(t, repo, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests":     []any{},
	}, ocispec.MediaTypeImageIndex)
	_, err = Seal(context.Background(), "registry.example.com/team/index@"+index.Digest.String(), Options{})
	if err == nil || !strings.Contains(err.Error(), "names an image index") {
		t.Fatalf("Seal err = %v, want an index decline", err)
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
