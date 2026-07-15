// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
)

// Inspect reports the OCI identity of an image present in the local store and
// the composefs binding recorded for it.
func (e *engine) Inspect(ctx context.Context, ref string) (*ImageInfo, error) {
	info, err := e.inspect(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", ref, err)
	}
	return info, nil
}

func (e *engine) inspect(ctx context.Context, ref string) (*ImageInfo, error) {
	root, err := e.resolveLocalRef(ref)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// A missing metadata blob means the image was never pulled for this host;
	// report it as ErrNotFound while preserving the underlying cause. Any other
	// resolve failure (platform mismatch, corrupt metadata, digest mismatch)
	// names the real conflict and propagates unchanged.
	res, err := ocichain.Resolve(ctx, casBlobSource{store: e.obj}, root, hostPlatform())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("not pulled for this platform; run `knarr pull` first: %w", errors.Join(ErrNotFound, err))
		}
		return nil, err
	}

	b, err := e.img.Get(root, res.Platform)
	if err != nil {
		if errors.Is(err, imagestore.ErrNotFound) {
			return nil, fmt.Errorf("no composefs image for this platform; run `knarr pull` first: %w", errors.Join(ErrNotFound, err))
		}
		return nil, fmt.Errorf("read image binding: %w", err)
	}

	return &ImageInfo{
		Root:           root,
		Platform:       res.Platform,
		DiffIDs:        res.DiffIDs,
		Composefs:      b.Descriptor,
		FsverityDigest: b.FsverityDigest,
	}, nil
}

// casBlobSource opens OCI metadata blobs by descriptor digest from the local
// content-addressed object store, satisfying ocichain.BlobSource.
type casBlobSource struct {
	store *objectstore.Store
}

func (s casBlobSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.store.Open(desc.Digest)
}
