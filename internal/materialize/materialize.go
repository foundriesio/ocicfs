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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/rootfs"
)

// Built is the image data materialized into the local stores. Root and RootDesc
// name the root the caller pinned, which may be an image index; Identity names
// the platform image manifest that root resolved to, the descriptor the binding
// is keyed by.
type Built struct {
	Root           digest.Digest
	Platform       ocispec.Platform
	Descriptor     ocispec.Descriptor
	FsverityDigest string
	RootDesc       ocispec.Descriptor
	Identity       ocispec.Descriptor
}

// Build is one image being materialized a layer at a time. It stores no
// context: every read runs under the context of the call that asked for it,
// and nothing a call starts outlives it.
type Build struct {
	store *objectstore.Store
	img   *imagestore.Store
	src   ocichain.BlobSource
	root  digest.Digest
	res   ocichain.Resolved
	b     *rootfs.Builder
	next  int // index of the next layer to ingest
}

// Begin does everything Materialize does before the first layer: verifies the
// store can enable fs-verity, resolves root from src for platform, writes the
// metadata blobs, and creates the rootfs builder. ctx governs this call's
// reads only. The caller must Finish or Close the build.
func Begin(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform) (*Build, error) {
	if store == nil {
		return nil, errors.New("materialize: nil object store")
	}
	if img == nil {
		return nil, errors.New("materialize: nil image store")
	}
	if src == nil {
		return nil, errors.New("materialize: nil blob source")
	}
	if err := store.CheckVerity(); err != nil {
		return nil, err
	}

	res, err := ocichain.Resolve(ctx, src, root, platform)
	if err != nil {
		return nil, err
	}
	if err := writeMetadataBlobs(ctx, store, res); err != nil {
		return nil, err
	}
	return &Build{store: store, img: img, src: src, root: root, res: res, b: rootfs.NewBuilder(store)}, nil
}

// Ingest applies every layer not yet applied up to and including i, in
// manifest order: each is opened through the blob source (its compressed
// digest verified as it is consumed), applied (its diff_id verified at end of
// stream), and closed before the next is opened. A jump forward applies every
// missing layer; an index at or below the last applied one does nothing; an
// index outside the manifest's layers is an error.
func (b *Build) Ingest(ctx context.Context, i int) error {
	if i < 0 || i >= len(b.res.DiffIDs) {
		return fmt.Errorf("materialize: layer index %d out of range for an image with %d layers", i, len(b.res.DiffIDs))
	}
	return b.ingestThrough(ctx, i)
}

// Finish ingests the remaining layers, finishes the rootfs build, verifies the
// sealing annotations, syncs the store, and records the binding under the
// resolved platform manifest. The build is spent afterwards.
func (b *Build) Finish(ctx context.Context) (Built, error) {
	if err := b.ingestThrough(ctx, len(b.res.DiffIDs)-1); err != nil {
		return Built{}, err
	}
	built, err := b.b.Finish(ctx)
	if err != nil {
		return Built{}, err
	}
	if err := ociseal.VerifyAnnotations(ctx, b.store, b.res, built.FsverityDigest, built.Canonical); err != nil {
		return Built{}, fmt.Errorf("materialize: verify sealing annotations: %w", err)
	}

	if err := b.store.Sync(); err != nil {
		return Built{}, err
	}
	if err := b.img.Put(b.res.ManifestDesc.Digest, b.res.Platform, imagestore.Binding{
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootfsSize:     built.RootfsSize,
		Canonical:      built.Canonical,
	}); err != nil {
		return Built{}, err
	}

	return Built{
		Root:           b.root,
		Platform:       b.res.Platform,
		Descriptor:     built.Descriptor,
		FsverityDigest: built.FsverityDigest,
		RootDesc:       b.res.RootDesc,
		Identity:       b.res.ManifestDesc,
	}, nil
}

// Close abandons a build that will not finish. Objects already sealed stay in
// the store as unreferenced content. Idempotent; a no-op after Finish.
func (b *Build) Close() {
	b.b.Close()
}

// ingestThrough applies layers next..i in order, opening, applying, and
// closing each before the next; a close failure is joined with the apply
// failure so neither is dropped.
func (b *Build) ingestThrough(ctx context.Context, i int) error {
	for ; b.next <= i; b.next++ {
		idx := b.next
		layer, err := ocichain.OpenLayer(ctx, b.src, b.res.Manifest.Layers[idx])
		if err != nil {
			return fmt.Errorf("materialize: open layer %d: %w", idx, err)
		}
		l, applyErr := b.b.Scan(ctx, layer, b.res.DiffIDs[idx], idx)
		if applyErr == nil {
			applyErr = b.b.Merge(l)
		}
		closeErr := layer.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("materialize: layer %d: close: %w", idx, closeErr)
		}
		if applyErr != nil || closeErr != nil {
			return errors.Join(applyErr, closeErr)
		}
	}
	return nil
}

// Materialize resolves root from src, builds the rootfs image, and records its
// binding in img under the resolved platform manifest, the digest that
// identifies the image however the caller named it. It is Begin followed by
// Finish.
func Materialize(ctx context.Context, store *objectstore.Store, img *imagestore.Store, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform) (Built, error) {
	b, err := Begin(ctx, store, img, src, root, platform)
	if err != nil {
		return Built{}, err
	}
	defer b.Close()
	return b.Finish(ctx)
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
