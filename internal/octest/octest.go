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

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Layout describes a written OCI image layout and the digests a pull test asserts
// against. Root is the pinned reference target; for a single-manifest layout it
// equals Manifest.
type Layout struct {
	Dir        string
	Ref        string
	Root       digest.Digest
	RootSize   int64
	Manifest   digest.Digest
	Config     digest.Digest
	FileDigest digest.Digest
	DiffIDs    []digest.Digest
	Platform   ocispec.Platform
}

// File is a regular file placed in the layout's single layer.
type File struct {
	Name string
	Mode int64
	Body string
}

// BuildSingleLayerGzip writes a minimal OCI image layout under dir: one tar+gzip
// layer holding files, a config for platform whose diff_id matches the layer, a
// manifest, and index.json. It returns the descriptors a pull test asserts against,
// with Ref pinning the manifest digest and FileDigest set to the content digest of
// files[0] (a per-file object a successful pull materializes into the CAS).
func BuildSingleLayerGzip(dir string, platform ocispec.Platform, files ...File) (Layout, error) {
	if len(files) == 0 {
		return Layout{}, fmt.Errorf("octest: need at least one file")
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o750); err != nil {
		return Layout{}, fmt.Errorf("octest: mkdir blobs: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0o600); err != nil {
		return Layout{}, fmt.Errorf("octest: write oci-layout: %w", err)
	}

	layer, err := tarBytes(files)
	if err != nil {
		return Layout{}, err
	}
	layerDesc, err := writeBlob(dir, gzipBytes(layer), ocispec.MediaTypeImageLayerGzip)
	if err != nil {
		return Layout{}, err
	}

	config := ocispec.Image{
		Platform: platform,
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{digest.SHA256.FromBytes(layer)},
		},
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
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return Layout{}, fmt.Errorf("octest: marshal manifest: %w", err)
	}
	manifestDesc, err := writeBlob(dir, manifestJSON, ocispec.MediaTypeImageManifest)
	if err != nil {
		return Layout{}, err
	}

	indexEntry := manifestDesc
	indexEntry.Platform = &platform
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{indexEntry},
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return Layout{}, fmt.Errorf("octest: marshal index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexJSON, 0o600); err != nil {
		return Layout{}, fmt.Errorf("octest: write index.json: %w", err)
	}

	return Layout{
		Dir:        dir,
		Ref:        "app@" + manifestDesc.Digest.String(),
		Root:       manifestDesc.Digest,
		RootSize:   manifestDesc.Size,
		Manifest:   manifestDesc.Digest,
		Config:     configDesc.Digest,
		FileDigest: digest.SHA256.FromBytes([]byte(files[0].Body)),
		DiffIDs:    config.RootFS.DiffIDs,
		Platform:   platform,
	}, nil
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

func gzipBytes(payload []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(payload) // bytes.Buffer writes never fail
	_ = gw.Close()
	return buf.Bytes()
}

func writeBlob(dir string, data []byte, mediaType string) (ocispec.Descriptor, error) {
	d := digest.SHA256.FromBytes(data)
	path := filepath.Join(dir, "blobs", "sha256", d.Hex())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("octest: write blob %s: %w", d, err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}, nil
}
