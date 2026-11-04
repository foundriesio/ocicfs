// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/rootfs"
	"github.com/foundriesio/ocicfs/pkg/version"
)

// layerSet holds, for every layer of the run, its inventory and the ocicfs
// scan record of it, each loaded from the cache or made by reading the
// layer's blob once from the spool.
type layerSet struct {
	dir        string // cache directory; "" when nothing is cached
	recordsRev string // the ocicfs revision ocicfs scan records are cached under; "" when they are not

	inv     map[string]*LayerInv
	records map[string][]byte // EncodeLayer bytes; nil for a layer too large to encode
	failed  map[string]error  // why the ocicfs scan rejected a layer
	blobs   map[string]blobRef

	invHits, recordHits, scans, ocicfsScans int
}

// blobRef is where a layer's blob can be read again.
type blobRef struct {
	src  ocichain.BlobSource
	desc ocispec.Descriptor
}

// newLayerSet returns an empty set caching under dir, or nowhere when dir is
// empty.
func newLayerSet(dir string) *layerSet {
	ls := &layerSet{
		dir:     dir,
		inv:     make(map[string]*LayerInv),
		records: make(map[string][]byte),
		failed:  make(map[string]error),
		blobs:   make(map[string]blobRef),
	}
	if dir != "" {
		ls.recordsRev = recordsRevision(version.Version, version.Revision)
	}
	return ls
}

// recordsRevision is the key ocicfs scan records are cached under: the
// stamped revision of a clean build, or "" for an unstamped or dirty one,
// whose ocicfs code may differ from any revision.
func recordsRevision(ver, rev string) string {
	if rev == "" || rev == "none" || ver == "" || ver == "dev" || strings.HasSuffix(ver, "-dirty") {
		return ""
	}
	return rev
}

// load makes the inventory and the ocicfs record of layer l available,
// reading its blob from src only for what the cache lacks. A layer the
// ocicfs scan rejects is remembered as failed rather than failing the load.
func (ls *layerSet) load(ctx context.Context, src ocichain.BlobSource, l LayerRef) error {
	desc := ocispec.Descriptor{MediaType: l.MediaType, Digest: digest.Digest(l.BlobDigest), Size: l.BlobSize}
	if _, ok := ls.blobs[l.DiffID]; !ok {
		ls.blobs[l.DiffID] = blobRef{src: src, desc: desc}
	}
	diffID := digest.Digest(l.DiffID)
	if _, ok := ls.inv[l.DiffID]; !ok {
		if inv := ls.readInv(diffID); inv != nil {
			ls.inv[l.DiffID] = inv
			ls.invHits++
		} else {
			inv, err := ls.scan(ctx, src, desc, diffID)
			if err != nil {
				return err
			}
			ls.inv[l.DiffID] = inv
			ls.writeInv(inv)
		}
	}
	if _, ok := ls.records[l.DiffID]; ok {
		return nil
	}
	if _, ok := ls.failed[l.DiffID]; ok {
		return nil
	}
	if data := ls.readRecord(diffID); data != nil {
		ls.records[l.DiffID] = data
		ls.recordHits++
		return nil
	}
	layer, err := ls.ocicfsScan(ctx, src, desc, diffID, 0)
	if err != nil {
		ls.failed[l.DiffID] = err
		return nil
	}
	data, err := rootfs.EncodeLayer(layer, 0, diffID)
	if err != nil {
		slog.Debug("layer too large to keep its ocicfs scan record", "diffID", diffID, "err", err)
		ls.records[l.DiffID] = nil
		return nil
	}
	ls.records[l.DiffID] = data
	ls.writeRecord(diffID, data)
	return nil
}

// ocicfsLayer returns layer diffID as ocicfs scans it, placed at position
// idx of a chain: decoded from its record, or scanned again when it has
// none. Every call returns a fresh layer, since merging consumes one.
func (ls *layerSet) ocicfsLayer(ctx context.Context, diffID string, idx int) (*rootfs.Layer, error) {
	if err := ls.failed[diffID]; err != nil {
		return nil, err
	}
	if data := ls.records[diffID]; data != nil {
		return rootfs.DecodeLayer(data, idx, 0, digest.Digest(diffID))
	}
	b, ok := ls.blobs[diffID]
	if !ok {
		return nil, fmt.Errorf("layer %s was never loaded", diffID)
	}
	return ls.ocicfsScan(ctx, b.src, b.desc, digest.Digest(diffID), idx)
}

// scan reads a layer blob into an inventory.
func (ls *layerSet) scan(ctx context.Context, src ocichain.BlobSource, desc ocispec.Descriptor, diffID digest.Digest) (*LayerInv, error) {
	r, err := ocichain.OpenLayer(ctx, src, desc)
	if err != nil {
		return nil, fmt.Errorf("open layer %s: %w", desc.Digest, err)
	}
	ls.scans++
	return scanLayer(ctx, r, diffID)
}

// ocicfsScan reads a layer blob through the ocicfs scan, hashing stored
// files instead of storing them.
func (ls *layerSet) ocicfsScan(ctx context.Context, src ocichain.BlobSource, desc ocispec.Descriptor, diffID digest.Digest, idx int) (*rootfs.Layer, error) {
	r, err := ocichain.OpenLayer(ctx, src, desc)
	if err != nil {
		return nil, fmt.Errorf("open layer %s: %w", desc.Digest, err)
	}
	ls.ocicfsScans++
	l, err := rootfs.ScanLayer(ctx, hashSink{}, r, diffID, idx)
	if cerr := r.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("close layer %s: %w", desc.Digest, cerr)
	}
	return l, err
}

func (ls *layerSet) invPath(diffID digest.Digest) string {
	return filepath.Join(ls.dir, "layers", "sha256", fmt.Sprintf("%s.s%d.gob.gz", diffID.Encoded(), scanSchema))
}

func (ls *layerSet) recordPath(diffID digest.Digest) string {
	return filepath.Join(ls.dir, "ocicfs-layers", "v2", diffID.Encoded()+"."+ls.recordsRev)
}

// readInv returns the cached inventory of diffID, or nil when there is none
// or it does not decode to one of this schema and layer.
func (ls *layerSet) readInv(diffID digest.Digest) *LayerInv {
	if ls.dir == "" {
		return nil
	}
	f, err := os.Open(ls.invPath(diffID))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	var inv LayerInv
	if err := gob.NewDecoder(zr).Decode(&inv); err != nil || inv.Schema != scanSchema || inv.DiffID != diffID.String() {
		return nil
	}
	return &inv
}

func (ls *layerSet) writeInv(inv *LayerInv) {
	if ls.dir == "" {
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	err := gob.NewEncoder(zw).Encode(inv)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = writeFileAtomic(ls.invPath(digest.Digest(inv.DiffID)), buf.Bytes())
	}
	if err != nil {
		slog.Warn("cache layer inventory", "diffID", inv.DiffID, "err", err)
	}
}

// readRecord returns the cached ocicfs scan record of diffID, or nil when
// there is none or it does not decode.
func (ls *layerSet) readRecord(diffID digest.Digest) []byte {
	if ls.recordsRev == "" {
		return nil
	}
	data, err := os.ReadFile(ls.recordPath(diffID))
	if err != nil {
		return nil
	}
	if _, err := rootfs.DecodeLayer(data, 0, 0, diffID); err != nil {
		return nil
	}
	return data
}

func (ls *layerSet) writeRecord(diffID digest.Digest, data []byte) {
	if ls.recordsRev == "" {
		return
	}
	if err := writeFileAtomic(ls.recordPath(diffID), data); err != nil {
		slog.Warn("cache ocicfs layer record", "diffID", diffID, "err", err)
	}
}

// hashSink hashes each stored file and discards its bytes. It reports the
// content digest in place of the fs-verity digest, which no sizing reads.
type hashSink struct{}

func (hashSink) WriteObject(_ context.Context, r io.Reader) (digest.Digest, objectstore.Sealed, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", nil, fmt.Errorf("hash object: %w", err)
	}
	d := digest.NewDigest(digest.SHA256, h)
	return d, objectstore.Resolved(d.String()), nil
}

// errNoCacheDir reports that no cache directory could be named.
var errNoCacheDir = errors.New("no cache directory: set XDG_CACHE_HOME or HOME, or pass --cache-dir or --no-cache")

// defaultCacheDir returns $XDG_CACHE_HOME/ocicfs-size.
func defaultCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", errNoCacheDir
	}
	return filepath.Join(dir, "ocicfs-size"), nil
}
