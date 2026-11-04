// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/rootfs"
)

// casRoot is one retained image as ocicfs prepares it: the merged rootfs
// entries, the identity of the composefs image built from them and that
// image's size.
type casRoot struct {
	img       *Image
	id        string // sha256 of the composefs dump
	canonical bool
	entries   []composefs.Entry
	size      int64
}

// prepareCASRoot merges an image's layers the way ocicfs does, from the
// layers' ocicfs scan records, and sizes the composefs image of the result
// with regular files of at most inlineMax bytes inline. The image is
// identified by the dump ocicfs writes: it carries the bytes of files ocicfs
// inlines and the content digest of the others, so it pins the image for
// any inlineMax.
func prepareCASRoot(ctx context.Context, ls *layerSet, img *Image, inlineMax int64) (*casRoot, error) {
	layers := make([]*rootfs.Layer, len(img.Layers))
	for i, l := range img.Layers {
		layer, err := ls.ocicfsLayer(ctx, l.DiffID, i)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.DiffID, err)
		}
		layers[i] = layer
	}
	entries, canonical, err := rootfs.MergeEntries(layers, func(d digest.Digest) digest.Digest { return d })
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	if err := composefs.WriteDump(h, entries); err != nil {
		return nil, err
	}
	size, err := composefsImageSize(entries, inlineMax)
	if err != nil {
		return nil, err
	}
	return &casRoot{img: img, id: hex.EncodeToString(h.Sum(nil)), canonical: canonical, entries: entries, size: size}, nil
}

// casSize estimates the ocicfs object store on ext4 for prepared roots:
// one composefs image per distinct dump, and one object per distinct
// digest among the files too large to inline and the roots' manifests,
// configs and indexes, every file sealed with fs-verity when the store
// seals. Its host metadata is the two sharded object namespaces and each
// root's binding and reference files.
func casSize(roots []*casRoot, p Profile) *Breakdown {
	b := &Breakdown{}
	seal := func(size int64) {
		if p.Verity {
			b.Verity += verityBytes(size, p.VerityBlock, p.HostBlock)
		}
	}
	payloads := map[string]int64{}
	images := map[string]bool{}
	for _, r := range roots {
		if images[r.id] {
			continue
		}
		images[r.id] = true
		file := alloc(r.size, p.HostBlock)
		b.Layout += file
		b.ImagePad += file - r.size
		seal(r.size)
		for _, e := range r.entries {
			switch {
			case e.Hardlink != "" || !e.Mode.IsRegular():
			case e.Content != nil && int64(len(e.Content)) <= p.InlineMax:
				b.Inline += int64(len(e.Content))
			case e.Content != nil:
				payloads[digest.FromBytes(e.Content).Encoded()] = int64(len(e.Content))
			case e.Size <= p.InlineMax:
				b.Inline += e.Size
			default:
				payloads[e.Digest.Encoded()] = e.Size
			}
		}
	}
	meta := map[string]int64{}
	for _, r := range roots {
		for d, size := range map[string]int64{r.img.IndexDigest: r.img.IndexSize, r.img.ManifestDigest: r.img.ManifestSize, r.img.ConfigDigest: r.img.ConfigSize} {
			if d == "" {
				continue
			}
			hexd := digest.Digest(d).Encoded()
			if _, ok := payloads[hexd]; !ok {
				meta[hexd] = size
			}
		}
	}
	names := make([]string, 0, len(payloads)+len(meta)+len(images))
	for d, size := range payloads {
		names = append(names, d)
		b.PayloadAlloc += alloc(size, p.HostBlock)
		b.PayloadBytes += size
		seal(size)
	}
	for d, size := range meta {
		names = append(names, d)
		b.OCIMeta += alloc(size, p.HostBlock)
		seal(size)
	}
	for id := range images {
		names = append(names, id)
	}
	blocks, files := casHostBlocks(roots, names)
	b.Host = blocks * p.HostBlock
	b.Files = int64(len(names)) + files
	b.Units = len(images)
	b.Objects = len(payloads)
	b.Reuse = casReuse(roots, p.InlineMax)
	b.total(p)
	return b
}

// casHostBlocks counts the object store's host blocks besides its files:
// the content namespace and its fs-verity view, both sharded, holding every
// object, and per root a binding file in a directory of its own and a
// reference file, each in sharded namespaces. The view namespace is keyed
// by fs-verity digests, which spread over its shards as content digests do.
// files counts the binding and reference files.
func casHostBlocks(roots []*casRoot, objects []string) (blocks, files int64) {
	blocks = 2 * shardedDirBlocks(objects, 0)
	var rootNames, refNames []string
	bindings := map[string]map[string]bool{}
	refs := map[string]bool{}
	for _, r := range roots {
		root := digest.Digest(r.img.ManifestDigest).Encoded()
		if r.img.IndexDigest != "" {
			root = digest.Digest(r.img.IndexDigest).Encoded()
		}
		if bindings[root] == nil {
			bindings[root] = map[string]bool{}
			rootNames = append(rootNames, root)
		}
		bindings[root][platformFile(r.img.Platform)] = true
		ref := digest.FromString(r.img.Ref).Encoded()
		if !refs[ref] {
			refs[ref] = true
			refNames = append(refNames, ref)
		}
	}
	for _, names := range bindings {
		lens := make([]int, 0, len(names))
		for f := range names {
			lens = append(lens, len(f))
		}
		blocks += ext4DirBlocks(lens) + int64(len(names))
		files += int64(len(names))
	}
	files += int64(len(refNames))
	return blocks + shardedDirBlocks(rootNames, 0) + shardedDirBlocks(refNames, len(".json")) + int64(len(refNames)), files
}

// platformFile names an image binding file after its platform.
func platformFile(platform string) string {
	b := []byte(platform)
	for i, c := range b {
		if c == '/' {
			b[i] = '-'
		}
	}
	return string(b) + ".json"
}
