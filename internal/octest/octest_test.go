// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package octest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func readBlob(t *testing.T, dir string, d digest.Digest) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", d.Hex())) // #nosec G304 -- test path from a digest the fixture wrote
	if err != nil {
		t.Fatalf("read blob %s: %v", d, err)
	}
	return data
}

// TestBuildImageLayerEncodings verifies that each supported layer media type
// is encoded accordingly on disk and that the recorded diff_id is the digest
// of the decoded tar stream.
func TestBuildImageLayerEncodings(t *testing.T) {
	linux := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	for _, tc := range []struct {
		mediaType string
		decode    func(t *testing.T, blob []byte) []byte
	}{
		{ocispec.MediaTypeImageLayerGzip, func(t *testing.T, blob []byte) []byte {
			t.Helper()
			gr, err := gzip.NewReader(bytes.NewReader(blob))
			if err != nil {
				t.Fatalf("gzip reader: %v", err)
			}
			raw, err := io.ReadAll(gr)
			if err != nil {
				t.Fatalf("gunzip: %v", err)
			}
			return raw
		}},
		{ocispec.MediaTypeImageLayerZstd, func(t *testing.T, blob []byte) []byte {
			t.Helper()
			zr, err := zstd.NewReader(bytes.NewReader(blob))
			if err != nil {
				t.Fatalf("zstd reader: %v", err)
			}
			defer zr.Close()
			raw, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("unzstd: %v", err)
			}
			return raw
		}},
		{ocispec.MediaTypeImageLayer, func(t *testing.T, blob []byte) []byte {
			t.Helper()
			return blob
		}},
		{"application/vnd.docker.image.rootfs.diff.tar.gzip", func(t *testing.T, blob []byte) []byte {
			t.Helper()
			gr, err := gzip.NewReader(bytes.NewReader(blob))
			if err != nil {
				t.Fatalf("gzip reader: %v", err)
			}
			raw, err := io.ReadAll(gr)
			if err != nil {
				t.Fatalf("gunzip: %v", err)
			}
			return raw
		}},
	} {
		t.Run(tc.mediaType, func(t *testing.T) {
			lay, err := BuildImage(t.TempDir(), ImageSpec{
				Platform: linux,
				Layers: []LayerSpec{{
					Files:     []File{{Name: "a.txt", Mode: 0o644, Body: "layer body"}},
					MediaType: tc.mediaType,
				}},
			})
			if err != nil {
				t.Fatalf("BuildImage: %v", err)
			}

			var manifest ocispec.Manifest
			if err := json.Unmarshal(readBlob(t, lay.Dir, lay.Manifest), &manifest); err != nil {
				t.Fatalf("parse manifest: %v", err)
			}
			if got := manifest.Layers[0].MediaType; got != tc.mediaType {
				t.Errorf("layer media type = %q, want %q", got, tc.mediaType)
			}
			raw := tc.decode(t, readBlob(t, lay.Dir, manifest.Layers[0].Digest))
			if got := digest.SHA256.FromBytes(raw); got != lay.DiffIDs[0] {
				t.Errorf("decoded tar digest = %s, want diff_id %s", got, lay.DiffIDs[0])
			}
		})
	}
}

// TestBuildImageMultiLayerHistory verifies multi-layer diff_id ordering and
// the history recorded in the config.
func TestBuildImageMultiLayerHistory(t *testing.T) {
	lay, err := BuildImage(t.TempDir(), ImageSpec{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Layers: []LayerSpec{
			{Files: []File{{Name: "base.txt", Mode: 0o644, Body: "base"}}},
			{Files: []File{{Name: "app.txt", Mode: 0o644, Body: "app"}}},
		},
		History: []ocispec.History{
			{CreatedBy: "ADD base /", EmptyLayer: false},
			{CreatedBy: "ENV A=1", EmptyLayer: true},
			{CreatedBy: "ADD app /", EmptyLayer: false},
		},
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if len(lay.DiffIDs) != 2 {
		t.Fatalf("diff_ids = %d, want 2", len(lay.DiffIDs))
	}

	var config ocispec.Image
	if err := json.Unmarshal(readBlob(t, lay.Dir, lay.Config), &config); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(config.History) != 3 || !config.History[1].EmptyLayer || config.History[2].CreatedBy != "ADD app /" {
		t.Fatalf("config history = %+v, want the 3 recorded entries", config.History)
	}
	for i, d := range config.RootFS.DiffIDs {
		if d != lay.DiffIDs[i] {
			t.Errorf("diff_id[%d] = %s, want %s", i, d, lay.DiffIDs[i])
		}
	}
}

// TestBuildImageMultiPlatformPinsIndex verifies that a multi-image layout is
// pinned by an index that is readable as a blob and references every platform.
func TestBuildImageMultiPlatformPinsIndex(t *testing.T) {
	files := []File{{Name: "a.txt", Mode: 0o644, Body: "x"}}
	lay, err := BuildImage(t.TempDir(),
		ImageSpec{Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"}, Layers: []LayerSpec{{Files: files}}},
		ImageSpec{Platform: ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, Layers: []LayerSpec{{Files: files}}},
	)
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if lay.Root == lay.Manifest {
		t.Fatalf("multi-image root = first manifest %s, want the index digest", lay.Root)
	}

	var index ocispec.Index
	if err := json.Unmarshal(readBlob(t, lay.Dir, lay.Root), &index); err != nil {
		t.Fatalf("parse index blob: %v", err)
	}
	if len(index.Manifests) != 2 {
		t.Fatalf("index manifests = %d, want 2", len(index.Manifests))
	}
	if index.Manifests[0].Digest != lay.Manifest {
		t.Errorf("index entry 0 = %s, want first manifest %s", index.Manifests[0].Digest, lay.Manifest)
	}
	for i, m := range index.Manifests {
		if m.Platform == nil {
			t.Errorf("index entry %d has no platform", i)
		}
	}
	// The Layout describes images[0]: the first platform.
	if lay.Platform.Architecture != "amd64" {
		t.Errorf("layout platform = %v, want images[0]'s linux/amd64", lay.Platform)
	}
}
