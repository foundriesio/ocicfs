// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociseal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

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

// TestVerifyAnnotations walks the merged-annotation matrix on both
// constructions: the annotation compared is the one selected by presence — the
// standard key, else the fallback key — whatever the canonical flag, and a
// non-canonical construction refuses the whole standard namespace first.
func TestVerifyAnnotations(t *testing.T) {
	store, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	const unrecognized = "composefs.merged.erofs.v1.fsverity-sha512-12"
	good, stale := strings.Repeat("a", 64), strings.Repeat("b", 64)
	measured := "sha256:" + good
	tests := []struct {
		name        string
		annotations map[string]string
		canonical   bool
		wantErr     string // substring of the expected error; empty means accepted
	}{
		{name: "canonical: none", canonical: true},
		{name: "canonical: unrelated annotation", canonical: true, annotations: map[string]string{"org.opencontainers.image.title": "app"}},
		{name: "canonical: standard match", canonical: true, annotations: map[string]string{MergedAnnotation: good}},
		{name: "canonical: standard mismatch", canonical: true, annotations: map[string]string{MergedAnnotation: stale}, wantErr: "mismatch"},
		{name: "canonical: standard malformed", canonical: true, annotations: map[string]string{MergedAnnotation: "not-a-digest"}, wantErr: "sealing annotation"},
		{name: "canonical: fallback match", canonical: true, annotations: map[string]string{FallbackMergedAnnotation: good}},
		{name: "canonical: fallback mismatch", canonical: true, annotations: map[string]string{FallbackMergedAnnotation: stale}, wantErr: "mismatch"},
		{name: "canonical: fallback malformed", canonical: true, annotations: map[string]string{FallbackMergedAnnotation: "not-a-digest"}, wantErr: "sealing annotation"},
		{name: "canonical: standard beside a stale fallback", canonical: true, annotations: map[string]string{MergedAnnotation: good, FallbackMergedAnnotation: stale}},
		{name: "canonical: standard beside a malformed fallback", canonical: true, annotations: map[string]string{MergedAnnotation: good, FallbackMergedAnnotation: "not-a-digest"}},
		{name: "canonical: stale standard beside a matching fallback", canonical: true, annotations: map[string]string{MergedAnnotation: stale, FallbackMergedAnnotation: good}, wantErr: "mismatch"},
		{name: "canonical: unrecognized kind alone", canonical: true, annotations: map[string]string{unrecognized: good}, wantErr: "unrecognized digest kind"},
		{name: "canonical: unrecognized kind beside standard", canonical: true, annotations: map[string]string{unrecognized: stale, MergedAnnotation: good}},
		{name: "canonical: unrecognized kind beside a stale standard", canonical: true, annotations: map[string]string{unrecognized: good, MergedAnnotation: stale}, wantErr: "mismatch"},
		{name: "canonical: unrecognized kind beside fallback", canonical: true, annotations: map[string]string{unrecognized: stale, FallbackMergedAnnotation: good}},

		{name: "non-canonical: none"},
		{name: "non-canonical: unrelated annotation", annotations: map[string]string{"org.opencontainers.image.title": "app"}},
		{name: "non-canonical: standard match", annotations: map[string]string{MergedAnnotation: good}, wantErr: "non-canonical"},
		{name: "non-canonical: unrecognized kind alone", annotations: map[string]string{unrecognized: good}, wantErr: "non-canonical"},
		{name: "non-canonical: config namespace only", annotations: map[string]string{ConfigAnnotation: good}},
		{name: "non-canonical: signature suffix", annotations: map[string]string{MergedAnnotation + ".sig": "not-a-digest"}, wantErr: "non-canonical"},
		{name: "non-canonical: fallback match", annotations: map[string]string{FallbackMergedAnnotation: good}},
		{name: "non-canonical: fallback mismatch", annotations: map[string]string{FallbackMergedAnnotation: stale}, wantErr: "mismatch"},
		{name: "non-canonical: fallback beside standard", annotations: map[string]string{FallbackMergedAnnotation: good, MergedAnnotation: good}, wantErr: "non-canonical"},
		{name: "non-canonical: fallback beside an unrecognized kind", annotations: map[string]string{FallbackMergedAnnotation: good, unrecognized: good}, wantErr: "non-canonical"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ocichain.Resolved{Manifest: ocispec.Manifest{Annotations: tt.annotations}}
			err := VerifyAnnotations(context.Background(), store, res, measured, tt.canonical)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyAnnotations: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("VerifyAnnotations error = %v, want %q", err, tt.wantErr)
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

func TestComputeBareManifest(t *testing.T) {
	requireMkcomposefs(t)
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout := octest.BuildIndexImage(t, platform, "value")
	src := openLayout(t, layout.Dir)

	result, manifests, err := Compute(context.Background(), src, layout.Root, nil, false)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if result.Original != layout.Root || result.Sealed == layout.Root {
		t.Fatalf("result = %#v, want changed root %s", result, layout.Root)
	}
	if len(result.Platforms) != 1 ||
		result.Platforms[0].Platform.OS != platform.OS ||
		result.Platforms[0].Platform.Architecture != platform.Architecture {
		t.Fatalf("platforms = %#v, want %#v", result.Platforms, platform)
	}
	if len(manifests) != 1 || !manifests[0].Changed || manifests[0].Digest != result.Sealed {
		t.Fatalf("manifests = %#v, want one changed sealed root", manifests)
	}
	if got := digest.FromBytes(manifests[0].Raw); got != manifests[0].Digest {
		t.Fatalf("manifest digest = %s, want %s", got, manifests[0].Digest)
	}
}

func TestComputeGoldenDigest(t *testing.T) {
	requireMkcomposefs(t)
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
	result, _, err := Compute(context.Background(), openLayout(t, layout.Dir), layout.Root, nil, false)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	const want = "sha256:9238176e13729b9215caf137866624e2f0d053c6e440e5446c824799d1d00123"
	if result.Platforms[0].MergedFsverity != want {
		t.Fatalf("merged fs-verity = %s, want %s", result.Platforms[0].MergedFsverity, want)
	}
}

func TestComputeIndexReturnsChildThenRoot(t *testing.T) {
	requireMkcomposefs(t)
	amd64 := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	layout, err := octest.BuildImage(t.TempDir(),
		octest.ImageSpec{Platform: amd64, Layers: []octest.LayerSpec{{Files: canonicalFiles("amd64")}}},
		octest.ImageSpec{Platform: arm64, Layers: []octest.LayerSpec{{Files: canonicalFiles("arm64")}}},
	)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	src := openLayout(t, layout.Dir)

	result, manifests, err := Compute(context.Background(), src, layout.Root, &amd64, false)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("manifests = %#v, want child and root", manifests)
	}
	if !manifests[0].Changed || !manifests[1].Changed {
		t.Fatalf("Changed = %v, %v, want true, true", manifests[0].Changed, manifests[1].Changed)
	}
	if manifests[1].Digest != result.Sealed || digest.FromBytes(manifests[1].Raw) != result.Sealed {
		t.Fatalf("last manifest = %#v, want root %s", manifests[1], result.Sealed)
	}
	var index ocispec.Index
	if err := json.Unmarshal(manifests[1].Raw, &index); err != nil {
		t.Fatalf("parse returned index: %v", err)
	}
	if index.Manifests[0].Digest != manifests[0].Digest {
		t.Fatalf("index child = %s, returned child %s", index.Manifests[0].Digest, manifests[0].Digest)
	}
}

func TestComputeUnchangedRootPreservesBytes(t *testing.T) {
	requireMkcomposefs(t)
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout, err := octest.BuildSingleLayerGzip(t.TempDir(), platform, canonicalFiles("value")...)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	base := openLayout(t, layout.Dir)
	first, manifests, err := Compute(context.Background(), base, layout.Root, nil, false)
	if err != nil {
		t.Fatalf("first Compute: %v", err)
	}
	src := &overlaySource{BlobSource: base, blobs: map[digest.Digest][]byte{first.Sealed: manifests[0].Raw}}

	result, unchanged, err := Compute(context.Background(), src, first.Sealed, nil, false)
	if err != nil {
		t.Fatalf("second Compute: %v", err)
	}
	if result.Sealed != first.Sealed || len(unchanged) != 1 || unchanged[0].Changed {
		t.Fatalf("result = %#v, manifests = %#v, want unchanged root %s", result, unchanged, first.Sealed)
	}
	if !bytes.Equal(unchanged[0].Raw, manifests[0].Raw) {
		t.Fatal("unchanged root bytes were not preserved")
	}

	t.Run("index", func(t *testing.T) {
		arm64 := ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
		layout, err := octest.BuildImage(t.TempDir(),
			octest.ImageSpec{Platform: platform, Layers: []octest.LayerSpec{{Files: canonicalFiles("amd64")}}},
			octest.ImageSpec{Platform: arm64, Layers: []octest.LayerSpec{{Files: canonicalFiles("arm64")}}},
		)
		if err != nil {
			t.Fatalf("BuildImage: %v", err)
		}
		base := openLayout(t, layout.Dir)
		first, manifests, err := Compute(context.Background(), base, layout.Root, &platform, false)
		if err != nil {
			t.Fatalf("first Compute: %v", err)
		}
		if len(manifests) != 2 {
			t.Fatalf("manifests = %#v, want changed child and index", manifests)
		}
		src := &overlaySource{BlobSource: base, blobs: map[digest.Digest][]byte{
			manifests[0].Digest: manifests[0].Raw,
			manifests[1].Digest: manifests[1].Raw,
		}}

		result, unchanged, err := Compute(context.Background(), src, first.Sealed, &platform, false)
		if err != nil {
			t.Fatalf("second Compute: %v", err)
		}
		if len(unchanged) != 1 {
			t.Fatalf("manifests = %#v, want one unchanged index", unchanged)
		}
		if unchanged[0].Changed {
			t.Fatal("index Changed = true, want false")
		}
		if unchanged[0].MediaType != ocispec.MediaTypeImageIndex {
			t.Fatalf("index media type = %q, want %q", unchanged[0].MediaType, ocispec.MediaTypeImageIndex)
		}
		if !bytes.Equal(unchanged[0].Raw, manifests[1].Raw) {
			t.Fatal("unchanged index bytes were not preserved")
		}
		if result.Sealed != first.Sealed {
			t.Fatalf("sealed = %s, want %s", result.Sealed, first.Sealed)
		}
	})
}

func TestComputeDeclinesNonCanonical(t *testing.T) {
	requireMkcomposefs(t)
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	layout := octest.NonCanonicalLayout(t, platform)
	src := openLayout(t, layout.Dir)

	_, _, err := Compute(context.Background(), src, layout.Root, nil, false)
	if !errors.Is(err, ErrNotCanonical) {
		t.Fatalf("Compute err = %v, want ErrNotCanonical", err)
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

type overlaySource struct {
	ocichain.BlobSource
	blobs map[digest.Digest][]byte
}

func (s *overlaySource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if raw, ok := s.blobs[desc.Digest]; ok {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
	return s.BlobSource.Open(ctx, desc)
}

func requireMkcomposefs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skipf("mkcomposefs unavailable: %v", err)
	}
}

func openLayout(t *testing.T, dir string) ocichain.BlobSource {
	t.Helper()
	src, err := ocilayout.Open(dir)
	if err != nil {
		t.Fatalf("ocilayout.Open: %v", err)
	}
	return src
}

func canonicalFiles(body string) []octest.File {
	return []octest.File{
		{Name: "usr", Dir: true},
		{Name: "etc", Dir: true},
		{Name: "empty", Mode: 0o644},
		{Name: "etc/value", Mode: 0o644, Body: body},
	}
}
