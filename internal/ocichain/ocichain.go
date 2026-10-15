// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ocichain verifies OCI descriptor chains and opens verified layer
// streams from a caller-provided blob source.
package ocichain

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/digestutil"
)

// BlobSource opens the raw blob bytes for a descriptor.
type BlobSource interface {
	Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error)
}

// RangeSource serves n bytes of a blob starting at offset off. The caller
// passes off >= 0, n >= 0 and off+n <= desc.Size; a source rejects anything
// else. A source that answers a range request with the whole blob reports
// whole=true and the entire stream verified against desc, which the caller
// consumes instead of the range. Either reader aborts once ctx is cancelled.
//
// No verification of a range's bytes is possible at this layer: a range is
// authenticated by what is built from it (a table of contents by its own
// checksum, a file by the digest that table records), never by the blob
// digest, which only covers the whole blob.
type RangeSource interface {
	OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (r io.ReadCloser, whole bool, err error)
}

// Resolved is the verified descriptor chain and authenticated metadata.
type Resolved struct {
	RootDesc      ocispec.Descriptor
	ManifestDesc  ocispec.Descriptor
	ConfigDesc    ocispec.Descriptor
	RootBytes     []byte
	ManifestBytes []byte
	ConfigBytes   []byte
	Manifest      ocispec.Manifest
	Config        ocispec.Image // Parsed form of ConfigBytes (platform, rootfs, history)
	Platform      ocispec.Platform
	DiffIDs       []digest.Digest // Extracted from config; verified to be sha256-only and count == layers
}

// ErrDigestMismatch reports that a metadata blob's bytes do not hash to the
// digest that named them: the stored or served content is corrupt, as opposed
// to merely absent or unparseable. Only the digest check reports it; a size
// disagreement is a plain error.
var ErrDigestMismatch = errors.New("ocichain: blob digest mismatch")

// Resolve verifies the descriptor chain from root for the wanted platform:
// root, manifest, and config bytes are authenticated against their digests, and
// the manifest's layer descriptors and the config's diff_ids are validated
// (sha256-only, matching counts). Layer blobs are not opened here; their
// content is verified by OpenLayer as it is consumed.
func Resolve(ctx context.Context, src BlobSource, root digest.Digest, want ocispec.Platform) (Resolved, error) {
	if src == nil {
		return Resolved{}, errors.New("ocichain: nil blob source")
	}

	// Step 1: Read and verify root blob. The root has no parent descriptor, so it
	// carries no declared size and is bounded by maxMetadataBytes.
	rootBytes, err := ReadRootBlob(ctx, src, root)
	if err != nil {
		return Resolved{}, err
	}
	rootDesc := ocispec.Descriptor{Digest: root, Size: int64(len(rootBytes))}

	isIndex, err := IsIndex(rootBytes)
	if err != nil {
		return Resolved{}, err
	}

	// For a bare manifest the root blob is itself the manifest; for an index we
	// select and read the platform's manifest out of it. Either way the manifest
	// descriptor and bytes then resolve through the same path.
	manifestDesc, manifestBytes := rootDesc, rootBytes
	if isIndex {
		var err error
		manifestDesc, manifestBytes, err = selectFromIndex(ctx, src, rootBytes, want)
		if err != nil {
			return Resolved{}, err
		}
	}
	return resolveFromManifest(ctx, src, rootDesc, manifestDesc, rootBytes, manifestBytes, want)
}

// OpenLayer opens the layer blob named by desc and returns a streaming reader
// over its decompressed contents (gzip, zstd, or uncompressed, auto-detected).
// The reader verifies the layer's compressed digest and size against desc at end
// of stream: a mismatch surfaces as a read error in place of a clean io.EOF, so a
// caller that consumes to EOF is forced to observe a corrupt or truncated layer.
// The blob is read in a single pass (no staged copy); the caller must Close the
// returned reader, which releases the decompressor and the source file.
//
// Only the compressed descriptor digest is verified here; the diff_id and any
// per-file checks belong to the step that materializes the layer.
func OpenLayer(ctx context.Context, src BlobSource, desc ocispec.Descriptor) (io.ReadCloser, error) {
	br, err := openVerifiedBlob(ctx, src, desc)
	if err != nil {
		return nil, err
	}
	if err := br.decompress(); err != nil {
		if closeErr := br.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("ocichain: OpenLayer close source: %w", closeErr))
		}
		return nil, err
	}
	return br, nil
}

// OpenBlob opens the blob named by desc and returns a streaming reader over its
// raw bytes, as stored: a compressed layer is not decompressed. The reader
// verifies the blob's digest and size against desc at end of stream, exactly as
// OpenLayer does: a mismatch surfaces as a read error in place of a clean
// io.EOF, so a caller that consumes to EOF is forced to observe a corrupt or
// truncated blob. The caller must Close the returned reader, which releases the
// source.
func OpenBlob(ctx context.Context, src BlobSource, desc ocispec.Descriptor) (io.ReadCloser, error) {
	br, err := openVerifiedBlob(ctx, src, desc)
	if err != nil {
		return nil, err
	}
	return br, nil
}

// ReadRootBlob reads, bounds, and digest-verifies the pinned root metadata blob
// a digest names directly — an image index or an image manifest. The root has no
// parent descriptor to vouch for its length, so it is bounded by the metadata
// cap rather than by an authenticated size. A source that can say something more
// useful than "not found" about a root it does not serve is asked to render the
// error, so every caller reading a pinned root gets that diagnostic.
func ReadRootBlob(ctx context.Context, src BlobSource, root digest.Digest) ([]byte, error) {
	data, err := readVerifyMetaBlob(ctx, src, ocispec.Descriptor{Digest: root}, sizeUnknown)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		if describer, ok := src.(missingRootDescriber); ok {
			return nil, describer.DescribeMissingRoot(root, err)
		}
	}
	return data, err
}

// ReadMetaBlob reads, bounds, and digest-verifies a metadata blob — a manifest
// or a config — named by a descriptor that came out of an already digest-verified
// parent. That parent declares the size, so it is authenticated: it bounds the
// read and is enforced exactly. Layer blobs are streamed by OpenLayer instead;
// they may be far larger than the metadata bound.
func ReadMetaBlob(ctx context.Context, src BlobSource, desc ocispec.Descriptor) ([]byte, error) {
	return readVerifyMetaBlob(ctx, src, desc, sizeRequired)
}

// IsIndex reports whether raw is an image index rather than an image manifest.
// Both carry schemaVersion 2, so the schema version alone does not distinguish
// them: an index media type is decisive, and otherwise the structure decides —
// an index carries manifest entries and no config. Letting the structure decide
// rather than trusting a manifest media type detects an index that wears one,
// and probing both fields avoids misclassifying a bare manifest as a
// single-entry index (or vice versa).
func IsIndex(raw []byte) (bool, error) {
	var probe struct {
		MediaType string               `json:"mediaType"`
		Manifests []ocispec.Descriptor `json:"manifests"`
		Config    ocispec.Descriptor   `json:"config"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false, fmt.Errorf("ocichain: parse metadata blob: %w", err)
	}
	switch probe.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		return true, nil
	}
	return len(probe.Manifests) > 0 && probe.Config.Digest == "", nil
}

// SelectManifestDesc returns the descriptor of the image manifest that raw, an
// image index, offers for the wanted platform. It reads nothing: the caller
// passes index bytes it has already digest-verified, and the returned
// descriptor — digest and the size the index declares for it — is authenticated
// by that verification. The match is exact under normalized platform equality
// and more than one matching entry is ambiguous, so an index is descended to
// exactly one manifest or to none. The selected digest is validated before it
// is returned, so the descriptor always names a sha256 blob.
func SelectManifestDesc(raw []byte, want ocispec.Platform) (ocispec.Descriptor, error) {
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("ocichain: parse index: %w", err)
	}

	var matches []ocispec.Descriptor
	for _, m := range index.Manifests {
		if m.Platform == nil {
			continue
		}
		if platformEquals(&want, m.Platform) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("ocichain: platform %s not found in index", platformString(&want))
	}
	if len(matches) > 1 {
		return ocispec.Descriptor{}, fmt.Errorf("ocichain: multiple matches for platform %s in index (ambiguous)", platformString(&want))
	}
	// This is where a selected digest first leaves decoded JSON, and a caller
	// that only selects — without going on to read the blob, whose read would
	// validate it — would otherwise carry an empty or malformed digest onwards
	// as if it named an image. An index naming one is malformed, which is a
	// different fault from a blob that fails to hash to its digest.
	if err := digestutil.ValidateSHA256(matches[0].Digest); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("ocichain: index entry for platform %s: %w", platformString(&want), err)
	}
	return matches[0], nil
}

type (
	missingRootDescriber interface {
		DescribeMissingRoot(root digest.Digest, cause error) error
	}

	// verifyingBlobReader streams a blob's bytes — raw, or decompressed once
	// decompress has installed a decoder — and, when the stream it serves
	// ends, drains and verifies the whole raw blob against its descriptor.
	verifyingBlobReader struct {
		desc        ocispec.Descriptor
		decomp      io.Reader     // what Read serves: the decoder, or the buffered raw blob itself when none is installed
		drain       *bufio.Reader // the buffered tee over the source; draining it pushes trailing raw bytes through the hasher
		hasher      hash.Hash
		counter     *byteCounter
		closeDecomp func() error // nil when no decoder is installed
		closers     []io.Closer
		verified    bool
		verifyErr   error
	}

	// byteCounter is an io.Writer that only tallies how many bytes pass through
	// it, used to measure the compressed layer size against the descriptor.
	byteCounter struct{ n int64 }

	// ctxReader makes a Read fail as soon as ctx is cancelled, so a large bounded
	// copy can be interrupted between reads rather than only at completion.
	ctxReader struct {
		ctx context.Context
		r   io.Reader
	}
)

// resolveFromManifest parses the selected manifest bytes and authenticates them
// into a Resolved: it reads and verifies the config, requires the config-derived
// platform to match the host, validates diff_ids and layers, and records that
// authenticated platform. Both the image-index and bare-manifest paths funnel
// through it, so it is the single place manifest bytes are unmarshalled.
func resolveFromManifest(ctx context.Context, src BlobSource, rootDesc, manifestDesc ocispec.Descriptor, rootBytes, manifestBytes []byte, want ocispec.Platform) (Resolved, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Resolved{}, fmt.Errorf("ocichain: parse manifest: %w", err)
	}
	if manifest.Config.Digest == "" {
		return Resolved{}, fmt.Errorf("ocichain: manifest has no config descriptor")
	}

	configDesc, configBytes, config, err := readVerifyConfigDesc(ctx, src, manifest.Config)
	if err != nil {
		return Resolved{}, err
	}

	platform := configPlatform(config)
	if !platformEquals(&want, platform) {
		return Resolved{}, fmt.Errorf("ocichain: image platform %s does not match host platform %s", platformString(platform), platformString(&want))
	}

	if err := validateDiffIDs(config.RootFS.DiffIDs, manifest.Layers); err != nil {
		return Resolved{}, err
	}
	if err := validateLayers(manifest.Layers); err != nil {
		return Resolved{}, err
	}

	return Resolved{
		RootDesc:      rootDesc,
		ManifestDesc:  manifestDesc,
		ConfigDesc:    configDesc,
		RootBytes:     rootBytes,
		ManifestBytes: manifestBytes,
		ConfigBytes:   configBytes,
		Manifest:      manifest,
		Config:        *config,
		Platform:      *platform,
		DiffIDs:       config.RootFS.DiffIDs,
	}, nil
}

// openVerifiedBlob opens desc from src and wraps the stream in a
// verifyingBlobReader over its raw bytes.
func openVerifiedBlob(ctx context.Context, src BlobSource, desc ocispec.Descriptor) (*verifyingBlobReader, error) {
	if src == nil {
		return nil, errors.New("ocichain: nil blob source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if desc.Size < 0 {
		return nil, fmt.Errorf("ocichain: blob descriptor has negative size %d", desc.Size)
	}

	r, err := src.Open(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("ocichain: open source: %w", err)
	}
	// ctxReader makes a long read abort promptly on cancellation.
	return newVerifyingBlobReader(&ctxReader{ctx: ctx, r: r}, desc, r), nil
}

// newVerifyingBlobReader wraps a raw blob stream r in a reader over the same
// bytes that verifies the blob's digest and size against desc when the stream
// ends. r is read at most desc.Size+1 bytes; every byte consumed is hashed and
// counted. A size or digest mismatch is surfaced as a read error in place of
// io.EOF, so a consumer that reads to completion cannot miss a corrupt or
// truncated blob.
//
// r is source-agnostic: a local file or a remote response body work identically.
// The returned reader's Close releases the supplied closers.
func newVerifyingBlobReader(r io.Reader, desc ocispec.Descriptor, closers ...io.Closer) *verifyingBlobReader {
	h := sha256.New()
	counter := &byteCounter{}
	// Bound the raw input to desc.Size+1 (so an oversized source is caught) and
	// tee every consumed byte into the hasher and the counter.
	teed := io.TeeReader(io.LimitReader(r, desc.Size+1), io.MultiWriter(h, counter))
	br := bufio.NewReader(teed)
	return &verifyingBlobReader{desc: desc, decomp: br, drain: br, hasher: h, counter: counter, closers: closers}
}

// decompress installs a decoder over the raw stream (gzip, zstd, or none,
// auto-detected from the magic) so that Read serves decompressed bytes. At end
// of the decoded stream the remaining raw bytes are drained through the hasher
// — load-bearing for a zstd:chunked layer, whose per-file TOC sits in a
// trailing skippable frame the zstd decoder need not consume — before the count
// and digest are checked.
func (lr *verifyingBlobReader) decompress() error {
	// Peek the magic without consuming it; a stream shorter than 4 bytes peeks
	// fewer and simply falls through to the uncompressed branch.
	header, _ := lr.drain.Peek(4)
	switch {
	case len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b:
		gr, err := gzip.NewReader(lr.drain)
		if err != nil {
			return fmt.Errorf("ocichain: OpenLayer gzip: %w", err)
		}
		lr.decomp = gr
		lr.closeDecomp = gr.Close
	case len(header) >= 4 && header[0] == 0x28 && header[1] == 0xb5 && header[2] == 0x2f && header[3] == 0xfd:
		zr, err := zstd.NewReader(lr.drain)
		if err != nil {
			return fmt.Errorf("ocichain: OpenLayer zstd: %w", err)
		}
		lr.decomp = zr
		// zstd.Decoder.Close releases its worker goroutines and returns nothing.
		lr.closeDecomp = func() error { zr.Close(); return nil }
	}
	return nil
}

func (lr *verifyingBlobReader) Read(p []byte) (int, error) {
	n, err := lr.decomp.Read(p)
	if errors.Is(err, io.EOF) {
		if vErr := lr.finish(); vErr != nil {
			return n, vErr
		}
	}
	return n, err
}

// finish drains any trailing compressed bytes through the hasher, then checks the
// accumulated size and digest against the descriptor. It runs once; later calls
// return the same verdict so repeated reads at EOF are stable.
func (lr *verifyingBlobReader) finish() error {
	if lr.verified {
		return lr.verifyErr
	}
	lr.verified = true
	// Drain remaining compressed input so the hash and count cover the whole blob
	// (e.g. a zstd:chunked layer's trailing skippable TOC frame).
	if _, err := io.Copy(io.Discard, lr.drain); err != nil {
		lr.verifyErr = fmt.Errorf("ocichain: drain blob: %w", err)
		return lr.verifyErr
	}
	switch {
	case lr.counter.n > lr.desc.Size:
		lr.verifyErr = fmt.Errorf("ocichain: blob source larger than descriptor size %d", lr.desc.Size)
	case lr.counter.n != lr.desc.Size:
		lr.verifyErr = fmt.Errorf("ocichain: blob size mismatch: expected %d, got %d", lr.desc.Size, lr.counter.n)
	default:
		// NewDigest formats the hasher's accumulated sum; it does not re-hash.
		if computed := digest.NewDigest(digest.SHA256, lr.hasher); computed.String() != lr.desc.Digest.String() {
			lr.verifyErr = fmt.Errorf("ocichain: blob digest mismatch: expected %s, got %s", lr.desc.Digest, computed)
		}
	}
	return lr.verifyErr
}

func (lr *verifyingBlobReader) Close() error {
	var err error
	if lr.closeDecomp != nil {
		err = errors.Join(err, lr.closeDecomp())
	}
	for _, c := range lr.closers {
		err = errors.Join(err, c.Close())
	}
	return err
}

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// maxMetadataBytes bounds the in-memory read of the root metadata blob, which has
// no parent descriptor to declare its size. Child blobs (manifest, config) are
// bounded instead by the size their digest-verified parent declares, so this cap
// applies only to the root.
const maxMetadataBytes = 32 << 20 // 32 MiB

// sizeRequired and sizeUnknown make readVerifyMetaBlob's call sites
// self-documenting. Every child descriptor (manifest, config) carries a size
// declared by its digest-verified parent and passes sizeRequired, which trusts
// that size as the read bound and enforces it exactly. Only the pinned root
// anchor, which has no parent descriptor and so no declared size, passes
// sizeUnknown and is bounded by maxMetadataBytes.
const (
	sizeRequired = true
	sizeUnknown  = false
)

// readVerifyMetaBlob reads an OCI metadata blob (index, manifest, or config)
// from the source, bounds it in size, verifies its digest, and returns its bytes.
// Use it strictly for metadata that is parsed into memory; it must not be used to
// read layer blobs, which are streamed and may be far larger than the metadata
// bound.
//
// When sizeRequired, the descriptor carries a size declared by its parent
// metablob, which was digest-verified before this descriptor was read out of it;
// that size is therefore authenticated and is honored directly as the read bound
// and enforced as the exact length (a non-positive size is malformed and
// rejected). Pass sizeUnknown only for the pinned root anchor, which has no parent
// descriptor to vouch for its size; it is read under maxMetadataBytes, the bound
// that stops a malformed layout from forcing an unbounded read before the digest
// is checked.
func readVerifyMetaBlob(ctx context.Context, src BlobSource, desc ocispec.Descriptor, sizeRequired bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d := desc.Digest
	// Validate before using the digest as a path component: this is the single
	// choke point where every metadata blob path is built, so a malformed or
	// path-bearing digest (e.g. sha256:../../x) is rejected here regardless of
	// caller.
	if err := digestutil.ValidateSHA256(d); err != nil {
		return nil, fmt.Errorf("ocichain: read blob: %w", err)
	}

	limit := int64(maxMetadataBytes)
	if sizeRequired {
		// The declared size comes from an already digest-verified parent, so it is
		// trustworthy as the read bound; honor it rather than clamping to the
		// root-only cap. A non-positive size is malformed: a real child blob always
		// declares a positive length.
		if desc.Size <= 0 {
			return nil, fmt.Errorf("ocichain: metadata blob %s has no positive declared size (%d)", d, desc.Size)
		}
		limit = desc.Size
	}

	r, err := src.Open(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("ocichain: read blob %s: %w", d, err)
	}
	defer warnClose("ocichain: failed to close metadata blob", r)

	// Hash the blob as it is read so its bytes are not walked a second time, and
	// read one byte past the limit so an over-size blob is distinguishable from
	// one sitting exactly at the limit.
	h := digest.SHA256.Hash()
	data, err := io.ReadAll(io.TeeReader(io.LimitReader(r, limit+1), h))
	if err != nil {
		return nil, fmt.Errorf("ocichain: read blob %s: %w", d, err)
	}
	if int64(len(data)) > limit {
		if sizeRequired {
			return nil, fmt.Errorf("ocichain: metadata blob %s is larger than its declared size %d", d, desc.Size)
		}
		return nil, fmt.Errorf("ocichain: metadata blob %s exceeds %d bytes", d, maxMetadataBytes)
	}
	// A child descriptor declares an exact size, so a short blob is malformed. The
	// digest check below would also reject it, but an explicit size error is
	// clearer and lets callers rely on the returned bytes matching desc.Size.
	if sizeRequired && int64(len(data)) != desc.Size {
		return nil, fmt.Errorf("ocichain: metadata blob %s size mismatch: expected %d, got %d", d, desc.Size, len(data))
	}

	// Verify the hash accumulated during the read against the expected digest.
	// NewDigest formats h's sum; it does not hash a second time.
	if computed := digest.NewDigest(digest.SHA256, h); computed != d {
		return nil, fmt.Errorf("%w for %s: computed %s", ErrDigestMismatch, d, computed)
	}

	return data, nil
}

// readVerifyConfigDesc reads and verifies config from a descriptor in the manifest.
func readVerifyConfigDesc(ctx context.Context, src BlobSource, configDesc ocispec.Descriptor) (ocispec.Descriptor, []byte, *ocispec.Image, error) {
	// ReadMetaBlob bounds, exact-size-checks, digest-validates, and
	// digest-verifies the blob.
	bytes, err := ReadMetaBlob(ctx, src, configDesc)
	if err != nil {
		return ocispec.Descriptor{}, nil, nil, err
	}

	var config ocispec.Image
	if err := json.Unmarshal(bytes, &config); err != nil {
		return ocispec.Descriptor{}, nil, nil, fmt.Errorf("ocichain: unmarshal config: %w", err)
	}

	return configDesc, bytes, &config, nil
}

// selectFromIndex selects the entry of the index in rootBytes that matches the
// host platform and reads and verifies its manifest blob. The manifest bytes are
// returned unparsed; resolveFromManifest unmarshals them.
func selectFromIndex(ctx context.Context, src BlobSource, rootBytes []byte, want ocispec.Platform) (ocispec.Descriptor, []byte, error) {
	selectedDesc, err := SelectManifestDesc(rootBytes, want)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	// Read and verify the manifest for this platform. readVerifyMetaBlob bounds,
	// exact-size-checks, digest-validates, and digest-verifies the blob.
	manifestBytes, err := ReadMetaBlob(ctx, src, selectedDesc)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	return selectedDesc, manifestBytes, nil
}

// platformEquals checks if two platforms are equal under normalized matching
// (e.g. arm64 ≡ arm64/v8, arm ≡ arm/v7). Some arches have a conventional default
// variant; an empty variant on either side is treated as that default so a
// variant-less host request (runtime.GOARCH yields no variant) still matches an
// image that names the default variant explicitly.
func platformEquals(a, b *ocispec.Platform) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.OS != b.OS || a.Architecture != b.Architecture {
		return false
	}
	def := defaultVariant(a.Architecture)
	aVar := a.Variant
	if aVar == "" {
		aVar = def
	}
	bVar := b.Variant
	if bVar == "" {
		bVar = def
	}
	return aVar == bVar
}

// validateLayers rejects the resolved chain before it is returned if any layer
// descriptor carries a non-sha256/malformed digest or a negative size, so a
// caller never sees a Resolved whose layer descriptors it cannot trust.
func validateLayers(layers []ocispec.Descriptor) error {
	for i, layer := range layers {
		if err := digestutil.ValidateSHA256(layer.Digest); err != nil {
			return fmt.Errorf("ocichain: layer[%d] descriptor: %w", i, err)
		}
		if layer.Size < 0 {
			return fmt.Errorf("ocichain: layer[%d] has negative size %d", i, layer.Size)
		}
	}
	return nil
}

// validateDiffIDs checks that the config's diff_ids count matches the manifest's
// layers and that every diff_id is a sha256 digest.
func validateDiffIDs(diffIDs []digest.Digest, layers []ocispec.Descriptor) error {
	if len(diffIDs) != len(layers) {
		return fmt.Errorf("ocichain: config has %d diff_ids but manifest has %d layers", len(diffIDs), len(layers))
	}
	for i, id := range diffIDs {
		if err := digestutil.ValidateSHA256(id); err != nil {
			return fmt.Errorf("ocichain: diff_id[%d]: %w", i, err)
		}
	}
	return nil
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// configPlatform derives the platform from an image config body, where the
// os/architecture/variant authoritatively live (the config descriptor's optional
// Platform field is absent in real images).
func configPlatform(c *ocispec.Image) *ocispec.Platform {
	return &ocispec.Platform{
		OS:           c.OS,
		Architecture: c.Architecture,
		Variant:      c.Variant,
	}
}

// defaultVariant returns the conventional default CPU variant for an
// architecture, or "" when the architecture has no single default.
func defaultVariant(arch string) string {
	switch arch {
	case "arm64":
		return "v8"
	case "arm":
		return "v7"
	default:
		return ""
	}
}

func platformString(p *ocispec.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func warnClose(msg string, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(msg, "err", err)
	}
}
