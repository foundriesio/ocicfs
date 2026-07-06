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

	layers, closers, err := openLayers(ctx, src, res.Manifest.Layers)
	if err != nil {
		return Built{}, err
	}
	built, buildErr := rootfs.Assemble(ctx, store, layers, res.DiffIDs)
	closeErr := closeLayers(closers)
	if buildErr != nil || closeErr != nil {
		return Built{}, errors.Join(buildErr, closeErr)
	}

	if err := store.Sync(); err != nil {
		return Built{}, err
	}
	if err := img.Put(root, res.Platform, imagestore.Binding{
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
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

func openLayers(ctx context.Context, src ocichain.BlobSource, descs []ocispec.Descriptor) ([]io.Reader, []io.Closer, error) {
	layers := make([]io.Reader, 0, len(descs))
	closers := make([]io.Closer, 0, len(descs))
	for _, desc := range descs {
		rc, err := ocichain.OpenLayer(ctx, src, desc)
		if err != nil {
			if closeErr := closeLayers(closers); closeErr != nil {
				return nil, nil, errors.Join(err, closeErr)
			}
			return nil, nil, err
		}
		layers = append(layers, rc)
		closers = append(closers, rc)
	}
	return layers, closers, nil
}

func closeLayers(closers []io.Closer) error {
	var err error
	for _, c := range closers {
		err = errors.Join(err, c.Close())
	}
	if err != nil {
		return fmt.Errorf("close OCI layer: %w", err)
	}
	return nil
}
