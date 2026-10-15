// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
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
	root, _, err := e.localAnchor(ref)
	if root == "" {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	res, err := e.resolveStoredImage(ctx, root, hostPlatform())
	if err != nil {
		return nil, err
	}

	b, err := e.img.Get(res.ManifestDesc.Digest, res.Platform)
	if err != nil {
		if errors.Is(err, imagestore.ErrNotFound) {
			return nil, fmt.Errorf("no composefs image for this platform; run `ocicfs pull` first: %w", errors.Join(ErrNotFound, err))
		}
		return nil, fmt.Errorf("read image binding: %w", err)
	}

	return &ImageInfo{
		Root:           res.ManifestDesc.Digest,
		Platform:       res.Platform,
		DiffIDs:        res.DiffIDs,
		Composefs:      b.Descriptor,
		FsverityDigest: b.FsverityDigest,
		RootfsSize:     b.RootfsSize,
		Layers:         layerTable(res),
	}, nil
}

// resolveStoredImage reads and hash-verifies the stored metadata chain that
// root anchors -- the index when root pins one, the platform image manifest,
// and its config -- in one resolution, the same one a pull performs. This is
// the first thing every reading verb does: everything after it keys on the
// verified result, never on the anchor alone. A missing blob means the image
// was never pulled for this host and is reported as ErrNotFound with the
// remedy; any other failure (digest mismatch, platform conflict, corrupt
// metadata) names the real conflict and propagates unchanged.
func (e *engine) resolveStoredImage(ctx context.Context, root digest.Digest, platform ocispec.Platform) (ocichain.Resolved, error) {
	res, err := ocichain.Resolve(ctx, casBlobSource{store: e.obj}, root, platform)
	if errors.Is(err, os.ErrNotExist) {
		return ocichain.Resolved{}, fmt.Errorf("not pulled for this platform; run `ocicfs pull` first: %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return ocichain.Resolved{}, err
	}
	return res, nil
}

// layerTable builds the per-layer view of a resolved chain: descriptor
// identity from the manifest, diff_ids from the config, chain IDs derived
// from the ordered diff_ids, and the history command correlated to each
// layer. It never mutates res: identity.ChainIDs writes its results back
// into the slice it is given, so the chain is computed over a copy.
func layerTable(res ocichain.Resolved) []LayerInfo {
	chainIDs := make([]digest.Digest, len(res.DiffIDs))
	copy(chainIDs, res.DiffIDs)
	identity.ChainIDs(chainIDs)

	createdBy := historyCommands(res.Config.History, len(res.Manifest.Layers))

	layers := make([]LayerInfo, len(res.Manifest.Layers))
	for i, l := range res.Manifest.Layers {
		layers[i] = LayerInfo{
			MediaType: l.MediaType,
			Digest:    l.Digest,
			Size:      l.Size,
			DiffID:    res.DiffIDs[i],
			ChainID:   chainIDs[i],
		}
		if createdBy != nil {
			layers[i].CreatedBy = createdBy[i]
		}
	}
	return layers
}

// historyCommands maps the config history onto the n filesystem layers:
// entries with empty_layer set describe config-only steps and consume no
// layer, so the k-th non-empty entry names layer k. History is optional and
// purely informational; when it is absent, or its non-empty entries do not
// number exactly n, nil is returned rather than guessing a shifted mapping.
func historyCommands(history []ocispec.History, n int) []string {
	if len(history) == 0 {
		return nil
	}
	cmds := make([]string, 0, n)
	for _, h := range history {
		if h.EmptyLayer {
			continue
		}
		cmds = append(cmds, h.CreatedBy)
	}
	if len(cmds) != n {
		return nil
	}
	return cmds
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
