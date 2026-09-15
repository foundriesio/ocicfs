// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package octest provides OCI layout, registry, and store-path fixtures shared
// by engine-level and CLI-level tests.
package octest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Layout describes a written OCI image layout and the digests a pull test asserts
// against. Root is the pinned reference target; for a single-manifest layout it
// equals Manifest. Layers holds the compressed layer digests in manifest order.
type Layout struct {
	Dir        string
	Ref        string
	Root       digest.Digest
	RootSize   int64
	Manifest   digest.Digest
	Config     digest.Digest
	FileDigest digest.Digest
	Layers     []digest.Digest
	DiffIDs    []digest.Digest
	Platform   ocispec.Platform
}

// File is a regular file placed in a layer, or, when Dir is set, a directory
// entry (Body and Link are ignored), or, when Link is set, a hardlink to the
// earlier entry named by Link (Mode and Body are ignored). Dir wins over Link.
type File struct {
	Name string
	Mode int64
	Body string
	Link string
	Dir  bool
}

// LayerSpec describes one layer of a built image: its files and the OCI layer
// media type, which selects how the tar stream is encoded on the wire (gzip,
// zstd, or plain tar). An empty MediaType defaults to tar+gzip.
type LayerSpec struct {
	Files     []File
	MediaType string
}

// ImageSpec describes one platform image of a built layout: its platform, its
// ordered layers, and the config history recorded for them.
type ImageSpec struct {
	Platform ocispec.Platform
	Layers   []LayerSpec
	History  []ocispec.History
}

// BuildImage writes an OCI image layout under dir holding one manifest per
// image, all referenced from index.json. A single image is pinned by its
// manifest digest; multiple images are pinned by the index digest, which is
// then also written as a blob so a pinned root resolves through the index.
// The returned Layout's Manifest, Config, Layers, DiffIDs, Platform, and
// FileDigest describe images[0] (FileDigest is left empty when its first layer holds no
// regular file).
func BuildImage(dir string, images ...ImageSpec) (Layout, error) {
	if len(images) == 0 {
		return Layout{}, fmt.Errorf("octest: need at least one image")
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o750); err != nil {
		return Layout{}, fmt.Errorf("octest: mkdir blobs: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0o600); err != nil {
		return Layout{}, fmt.Errorf("octest: write oci-layout: %w", err)
	}

	var (
		entries       []ocispec.Descriptor
		firstManifest ocispec.Descriptor
		out           Layout
	)
	for i, img := range images {
		if len(img.Layers) == 0 {
			return Layout{}, fmt.Errorf("octest: image %d needs at least one layer", i)
		}

		layerDescs := make([]ocispec.Descriptor, 0, len(img.Layers))
		diffIDs := make([]digest.Digest, 0, len(img.Layers))
		for _, l := range img.Layers {
			raw, err := tarBytes(l.Files)
			if err != nil {
				return Layout{}, err
			}
			mediaType := l.MediaType
			if mediaType == "" {
				mediaType = ocispec.MediaTypeImageLayerGzip
			}
			encoded, err := encodeLayer(raw, mediaType)
			if err != nil {
				return Layout{}, err
			}
			desc, err := writeBlob(dir, encoded, mediaType)
			if err != nil {
				return Layout{}, err
			}
			layerDescs = append(layerDescs, desc)
			diffIDs = append(diffIDs, digest.SHA256.FromBytes(raw))
		}

		config := ocispec.Image{
			Platform: img.Platform,
			RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
			History:  img.History,
		}
		configJSON, err := json.Marshal(config)
		if err != nil {
			return Layout{}, fmt.Errorf("octest: marshal config: %w", err)
		}
		configDesc, err := writeBlob(dir, configJSON, ocispec.MediaTypeImageConfig)
		if err != nil {
			return Layout{}, err
		}

		manifest := ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    configDesc,
			Layers:    layerDescs,
		}
		manifestJSON, err := json.Marshal(manifest)
		if err != nil {
			return Layout{}, fmt.Errorf("octest: marshal manifest: %w", err)
		}
		manifestDesc, err := writeBlob(dir, manifestJSON, ocispec.MediaTypeImageManifest)
		if err != nil {
			return Layout{}, err
		}

		entry := manifestDesc
		platform := img.Platform
		entry.Platform = &platform
		entries = append(entries, entry)

		if i == 0 {
			firstManifest = manifestDesc
			out = Layout{
				Dir:      dir,
				Manifest: manifestDesc.Digest,
				Config:   configDesc.Digest,
				DiffIDs:  diffIDs,
				Platform: img.Platform,
			}
			for _, d := range layerDescs {
				out.Layers = append(out.Layers, d.Digest)
			}
			for _, f := range img.Layers[0].Files {
				if f.Dir || f.Link != "" {
					continue
				}
				out.FileDigest = digest.SHA256.FromBytes([]byte(f.Body))
				break
			}
		}
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: entries,
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return Layout{}, fmt.Errorf("octest: marshal index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexJSON, 0o600); err != nil {
		return Layout{}, fmt.Errorf("octest: write index.json: %w", err)
	}

	root := firstManifest
	if len(images) > 1 {
		indexDesc, err := writeBlob(dir, indexJSON, ocispec.MediaTypeImageIndex)
		if err != nil {
			return Layout{}, err
		}
		root = indexDesc
	}
	out.Ref = "app@" + root.Digest.String()
	out.Root = root.Digest
	out.RootSize = root.Size
	return out, nil
}

// BuildSingleLayerGzip writes a minimal OCI image layout under dir: one tar+gzip
// layer holding files, a config for platform whose diff_id matches the layer, a
// manifest, and index.json. It returns the descriptors a pull test asserts against,
// with Ref pinning the manifest digest and FileDigest set to the content digest of
// the first regular file (a per-file object a successful pull materializes into the
// CAS when the file exceeds the inline threshold; smaller files are inlined and
// store no object; empty when the layer holds no regular file).
func BuildSingleLayerGzip(dir string, platform ocispec.Platform, files ...File) (Layout, error) {
	if len(files) == 0 {
		return Layout{}, fmt.Errorf("octest: need at least one file")
	}
	return BuildImage(dir, ImageSpec{Platform: platform, Layers: []LayerSpec{{Files: files}}})
}

// ThreeLayerImage builds a three-layer image for platform under a fresh temp
// directory. Every layer stores a regular file over the inline threshold, so
// each layer materializes an object.
func ThreeLayerImage(t testing.TB, platform ocispec.Platform) Layout {
	t.Helper()
	body := func(n int) string {
		return fmt.Sprintf("layer %d payload larger than the sixty-four byte inline limit so it becomes an object\n", n)
	}
	lay, err := BuildImage(t.TempDir(), ImageSpec{
		Platform: platform,
		Layers: []LayerSpec{
			{Files: []File{{Name: "usr", Dir: true}, {Name: "usr/bin", Dir: true}, {Name: "usr/bin/app", Mode: 0o755, Body: body(0)}}},
			{Files: []File{{Name: "usr/lib", Dir: true}, {Name: "usr/lib/one", Mode: 0o644, Body: body(1)}}},
			{Files: []File{{Name: "etc", Dir: true}, {Name: "etc/two", Mode: 0o644, Body: body(2)}}},
		},
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	return lay
}

// NonCanonicalLayout builds a single-layer image whose tree omits /usr, so
// canonical construction must substitute default root metadata.
func NonCanonicalLayout(t testing.TB, platform ocispec.Platform) Layout {
	t.Helper()
	lay, err := BuildSingleLayerGzip(t.TempDir(), platform,
		File{Name: "etc/value", Mode: 0o644, Body: "value"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	return lay
}

// BuildIndexImage builds a canonical single-layer image used as an index
// child.
func BuildIndexImage(t testing.TB, platform ocispec.Platform, body string) Layout {
	t.Helper()
	lay, err := BuildSingleLayerGzip(t.TempDir(), platform,
		File{Name: "usr", Dir: true},
		File{Name: "etc", Dir: true},
		File{Name: "empty", Mode: 0o644},
		File{Name: "etc/platform", Mode: 0o644, Body: body},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	return lay
}

// ObjectPath returns the CAS path a stored object with digest d lands at.
func ObjectPath(root string, d digest.Digest) string {
	return filepath.Join(root, "objects", "sha256", d.Hex()[:2], d.Hex()[2:])
}

// BindingPath returns the image-binding path for root digest d and platform p.
func BindingPath(root string, d digest.Digest, p ocispec.Platform) string {
	name := strings.ToLower(p.OS) + "-" + strings.ToLower(p.Architecture)
	if p.Variant != "" {
		name += "-" + strings.ToLower(p.Variant)
	}
	return filepath.Join(root, "images", "sha256", d.Hex()[:2], d.Hex()[2:], name+".json")
}

// RefPath returns the ref-record path for the exact reference string ref.
func RefPath(root, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	key := hex.EncodeToString(sum[:])
	return filepath.Join(root, "refs", "sha256", key[:2], key[2:]+".json")
}

func tarBytes(files []File) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Real image layers open with a "./" root directory entry; include it so the
	// fixture exercises the same root-entry handling as production images.
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, fmt.Errorf("octest: tar root header: %w", err)
	}
	for _, f := range files {
		if f.Dir {
			mode := f.Mode
			if mode == 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Name: f.Name, Typeflag: tar.TypeDir, Mode: mode}); err != nil {
				return nil, fmt.Errorf("octest: tar header %s: %w", f.Name, err)
			}
			continue
		}
		if f.Link != "" {
			hdr := &tar.Header{Name: f.Name, Typeflag: tar.TypeLink, Linkname: f.Link}
			if err := tw.WriteHeader(hdr); err != nil {
				return nil, fmt.Errorf("octest: tar header %s: %w", f.Name, err)
			}
			continue
		}
		hdr := &tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Body))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("octest: tar header %s: %w", f.Name, err)
		}
		if _, err := tw.Write([]byte(f.Body)); err != nil {
			return nil, fmt.Errorf("octest: tar write %s: %w", f.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("octest: tar close: %w", err)
	}
	return buf.Bytes(), nil
}

// encodeLayer encodes a raw tar stream in the on-the-wire form the layer
// media type (OCI or Docker-schema spelling) declares.
func encodeLayer(raw []byte, mediaType string) ([]byte, error) {
	switch mediaType {
	case ocispec.MediaTypeImageLayerGzip, "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return gzipBytes(raw), nil
	case ocispec.MediaTypeImageLayerZstd:
		return zstdBytes(raw)
	case ocispec.MediaTypeImageLayer, "application/vnd.docker.image.rootfs.diff.tar":
		return raw, nil
	default:
		return nil, fmt.Errorf("octest: unsupported layer media type %q", mediaType)
	}
}

func gzipBytes(payload []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(payload) // bytes.Buffer writes never fail
	_ = gw.Close()
	return buf.Bytes()
}

func zstdBytes(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, fmt.Errorf("octest: zstd writer: %w", err)
	}
	_, _ = zw.Write(payload) // bytes.Buffer writes never fail
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("octest: zstd close: %w", err)
	}
	return buf.Bytes(), nil
}

func writeBlob(dir string, data []byte, mediaType string) (ocispec.Descriptor, error) {
	d := digest.SHA256.FromBytes(data)
	path := filepath.Join(dir, "blobs", "sha256", d.Hex())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("octest: write blob %s: %w", d, err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}, nil
}
