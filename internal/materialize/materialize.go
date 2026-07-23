// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package materialize builds and records a prepared image from verified OCI
// blobs.
package materialize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/rootfs"
)

// Built is the image data materialized into the local stores.
type Built struct {
	Root           digest.Digest
	Platform       ocispec.Platform
	Descriptor     ocispec.Descriptor
	FsverityDigest string
	RootDesc       ocispec.Descriptor
}

// Materialize resolves root from src, builds the rootfs image, and records its
// binding in img.
func Materialize(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform) (Built, error) {
	if store == nil {
		return Built{}, errors.New("materialize: nil object store")
	}
	if img == nil {
		return Built{}, errors.New("materialize: nil image store")
	}
	if src == nil {
		return Built{}, errors.New("materialize: nil blob source")
	}
	if err := store.CheckVerity(); err != nil {
		return Built{}, err
	}

	res, err := ocichain.Resolve(ctx, src, root, platform)
	if err != nil {
		return Built{}, err
	}
	if err := writeMetadataBlobs(ctx, store, res); err != nil {
		return Built{}, err
	}

	openLayer := func(i int) (io.ReadCloser, error) {
		return ocichain.OpenLayer(ctx, src, res.Manifest.Layers[i])
	}
	built, err := rootfs.Assemble(ctx, store, openLayer, res.DiffIDs)
	if err != nil {
		return Built{}, err
	}
	if err := ociseal.VerifyAnnotations(ctx, store, res, built.FsverityDigest, built.Canonical); err != nil {
		return Built{}, fmt.Errorf("materialize: verify sealing annotations: %w", err)
	}

	if err := store.Sync(); err != nil {
		return Built{}, err
	}
	if err := img.Put(root, res.Platform, imagestore.Binding{
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootfsSize:     built.RootfsSize,
		Canonical:      built.Canonical,
	}); err != nil {
		return Built{}, err
	}

	return Built{
		Root:           root,
		Platform:       res.Platform,
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootDesc:       res.RootDesc,
	}, nil
}

func writeMetadataBlobs(ctx context.Context, store *objectstore.Store, res ocichain.Resolved) error {
	seen := make(map[digest.Digest]bool, 3)
	if err := writeMetaBlob(ctx, store, seen, res.RootDesc, res.RootBytes); err != nil {
		return fmt.Errorf("write root metadata: %w", err)
	}
	if err := writeMetaBlob(ctx, store, seen, res.ManifestDesc, res.ManifestBytes); err != nil {
		return fmt.Errorf("write manifest metadata: %w", err)
	}
	if err := writeMetaBlob(ctx, store, seen, res.ConfigDesc, res.ConfigBytes); err != nil {
		return fmt.Errorf("write config metadata: %w", err)
	}
	return nil
}

func writeMetaBlob(ctx context.Context, store *objectstore.Store, seen map[digest.Digest]bool, desc ocispec.Descriptor, data []byte) error {
	if seen[desc.Digest] {
		return nil
	}
	seen[desc.Digest] = true
	return store.WriteBlob(ctx, desc.Digest, bytes.NewReader(data))
}
