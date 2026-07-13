// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocichain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type fixture struct {
	t     *testing.T
	blobs map[digest.Digest][]byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, blobs: make(map[digest.Digest][]byte)}
}

func (f *fixture) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := f.blobs[desc.Digest]
	if !ok {
		return nil, fmt.Errorf("missing blob %s: %w", desc.Digest, os.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// writeBlob stores raw bytes and returns its descriptor.
func (f *fixture) writeBlob(data []byte, mediaType string) ocispec.Descriptor {
	f.t.Helper()
	d := digest.SHA256.FromBytes(data)
	f.blobs[d] = append([]byte(nil), data...)
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}
}

func (f *fixture) setBlob(d digest.Digest, data []byte) {
	f.t.Helper()
	f.blobs[d] = append([]byte(nil), data...)
}

func (f *fixture) flipBlob(d digest.Digest) {
	f.t.Helper()
	b, ok := f.blobs[d]
	if !ok {
		f.t.Fatalf("blob %s missing", d)
	}
	if len(b) == 0 {
		f.t.Fatalf("blob %s is empty", d)
	}
	flipped := append([]byte(nil), b...)
	flipped[len(flipped)-1] ^= 0xff
	f.blobs[d] = flipped
}

// fakeLayerDesc returns a layer descriptor for content that is NOT written to
// disk. Resolve never reads layer blobs (it reads only root/manifest/config),
// so resolve-level tests can reference layers by descriptor alone.
func fakeLayerDesc(content string) ocispec.Descriptor {
	data := []byte(content)
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerZstd,
		Digest:    digest.SHA256.FromBytes(data),
		Size:      int64(len(data)),
	}
}

// configJSON builds a raw image-config document with the given platform and
// diff_ids, so tests control os/architecture/variant and the diff_ids list
// (including malformed values) exactly.
func configJSON(osName, arch, variant string, diffIDs []string) []byte {
	m := map[string]any{
		"architecture": arch,
		"os":           osName,
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": diffIDs,
		},
	}
	if variant != "" {
		m["variant"] = variant
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func manifestJSON(config ocispec.Descriptor, layers []ocispec.Descriptor) []byte {
	man := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    layers,
	}
	b, err := json.Marshal(man)
	if err != nil {
		panic(err)
	}
	return b
}

func indexJSON(entries []ocispec.Descriptor) []byte {
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: entries,
	}
	b, err := json.Marshal(idx)
	if err != nil {
		panic(err)
	}
	return b
}

// diffIDsFor returns one syntactically valid (sha256) diff_id per layer.
func diffIDsFor(layers []ocispec.Descriptor) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		// A diff_id is the digest of the decompressed layer; for resolve-level
		// tests any well-formed sha256 value of the right count suffices.
		out[i] = digest.SHA256.FromString("diff-" + l.Digest.Hex()).String()
	}
	return out
}

// --- Resolve: index path ---

func TestResolveIndexSelectsPlatform(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("layer-a")}

	amdCfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	amdMan := f.writeBlob(manifestJSON(amdCfg, layers), ocispec.MediaTypeImageManifest)
	amdMan.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}

	armCfg := f.writeBlob(configJSON("linux", "arm64", "v8", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	armMan := f.writeBlob(manifestJSON(armCfg, layers), ocispec.MediaTypeImageManifest)
	armMan.Platform = &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}

	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{amdMan, armMan}), ocispec.MediaTypeImageIndex)

	res, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Platform.Architecture != "arm64" {
		t.Fatalf("selected platform = %q, want arm64", res.Platform.Architecture)
	}
	if res.ManifestDesc.Digest != armMan.Digest {
		t.Fatalf("selected manifest = %s, want %s", res.ManifestDesc.Digest, armMan.Digest)
	}
	if res.ConfigDesc.Digest != armCfg.Digest {
		t.Fatalf("selected config = %s, want %s", res.ConfigDesc.Digest, armCfg.Digest)
	}
	if res.RootDesc.Digest != idx.Digest {
		t.Fatalf("root desc = %s, want %s", res.RootDesc.Digest, idx.Digest)
	}
	if len(res.DiffIDs) != len(layers) {
		t.Fatalf("diff_ids = %d, want %d", len(res.DiffIDs), len(layers))
	}
}

func TestResolveIndexNormalizedVariantMatch(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	cfg := f.writeBlob(configJSON("linux", "arm64", "v8", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
	man.Platform = &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{man}), ocispec.MediaTypeImageIndex)

	if _, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "arm64"}); err != nil {
		t.Fatalf("normalized arm64 == arm64/v8 match should resolve: %v", err)
	}
}

func TestResolveIndexAmbiguousMatchRejected(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
	man.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}

	// Two entries match linux/amd64 — ambiguous.
	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{man, man}), ocispec.MediaTypeImageIndex)

	_, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected ambiguous-match rejection")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous-match", err)
	}
}

func TestResolveIndexPlatformAbsent(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
	man.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{man}), ocispec.MediaTypeImageIndex)

	_, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err == nil {
		t.Fatal("expected not-found for absent platform")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want not-found", err)
	}
}

func TestResolveIndexEntryVsConfigPlatformDisagreementRejected(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	// Config says amd64...
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
	// ...but the index entry claims arm64.
	man.Platform = &ocispec.Platform{OS: "linux", Architecture: "arm64"}
	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{man}), ocispec.MediaTypeImageIndex)

	_, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err == nil {
		t.Fatal("expected rejection of index-entry vs config platform disagreement")
	}
	if !strings.Contains(err.Error(), "does not match host") {
		t.Fatalf("error = %v, want host-platform mismatch", err)
	}
}

// --- Resolve: bare-manifest path ---

func TestResolveBareManifestDerivesPlatform(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l1"), fakeLayerDesc("l2")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	res, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("Resolve bare manifest: %v", err)
	}
	if res.Platform.OS != "linux" || res.Platform.Architecture != "amd64" {
		t.Fatalf("derived platform = %v, want linux/amd64", res.Platform)
	}
	// For a bare manifest, root IS the manifest.
	if res.ManifestDesc.Digest != man.Digest || res.RootDesc.Digest != man.Digest {
		t.Fatalf("bare manifest: manifest/root desc mismatch")
	}
	if res.ConfigDesc.Digest != cfg.Digest {
		t.Fatalf("config desc = %s, want %s", res.ConfigDesc.Digest, cfg.Digest)
	}
	if len(res.DiffIDs) != 2 {
		t.Fatalf("diff_ids = %d, want 2", len(res.DiffIDs))
	}
}

func TestResolveBareManifestNonHostRejected(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	// Config is arm64; host is asked as amd64 — S0 is host-platform only.
	cfg := f.writeBlob(configJSON("linux", "arm64", "v8", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	_, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of non-host bare manifest")
	}
	if !strings.Contains(err.Error(), "does not match host") {
		t.Fatalf("error = %v, want host-platform mismatch", err)
	}
}

// --- Resolve: malformed inputs ---

func TestResolveNonSHA256ConfigDescriptorRejected(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	cfgBytes := configJSON("linux", "amd64", "", diffIDsFor(layers))
	// Reference the config with a non-sha256 algorithm.
	badCfg := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.Digest("sha512:" + strings.Repeat("a", 128)),
		Size:      int64(len(cfgBytes)),
	}
	man := f.writeBlob(manifestJSON(badCfg, layers), ocispec.MediaTypeImageManifest)

	_, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of non-sha256 config descriptor")
	}
	if !strings.Contains(err.Error(), "non-sha256") {
		t.Fatalf("error = %v, want non-sha256 rejection", err)
	}
}

func TestResolveDiffIDsCountMismatchRejected(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l1"), fakeLayerDesc("l2")}
	// Only one diff_id for two layers.
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers[:1])), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	_, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of diff_ids count mismatch")
	}
	if !strings.Contains(err.Error(), "diff_ids") {
		t.Fatalf("error = %v, want diff_ids count mismatch", err)
	}
}

func TestResolveNonSHA256DiffIDRejected(t *testing.T) {
	f := newFixture(t)

	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	// A syntactically valid but non-sha256 diff_id.
	badDiff := []string{"sha512:" + strings.Repeat("b", 128)}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", badDiff), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	_, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of non-sha256 diff_id")
	}
	if !strings.Contains(err.Error(), "non-sha256") {
		t.Fatalf("error = %v, want non-sha256 diff_id rejection", err)
	}
}

func TestResolveCorruptRootBlobRejected(t *testing.T) {
	f := newFixture(t)

	// Store bytes under a digest they do NOT hash to, so reading the blob by
	// that digest fails verification.
	wrong := digest.SHA256.FromString("not-the-content")
	f.setBlob(wrong, []byte(`{"schemaVersion":2,"tampered":true}`))

	_, err := Resolve(context.Background(), f, wrong, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of corrupt root blob")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
}

func TestResolveRejectsNonSHA256Root(t *testing.T) {
	f := newFixture(t)
	_, err := Resolve(context.Background(), f, digest.Digest("sha1:invalid"),
		ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected error for non-sha256 root digest")
	}
}

// --- OpenLayer: streaming decompress + verify-at-EOF ---

// openLayerRaw writes raw as a layer blob and opens it; the descriptor digest is
// taken over raw, so a faithful read verifies and a tampered blob does not.
func openLayerRaw(t *testing.T, f *fixture, raw []byte) (BlobSource, ocispec.Descriptor) {
	t.Helper()
	desc := f.writeBlob(raw, ocispec.MediaTypeImageLayer)
	return f, desc
}

// readOpened opens the layer and returns its fully decompressed bytes, asserting
// no read error (verification passed at EOF).
func readOpened(t *testing.T, src BlobSource, desc ocispec.Descriptor) []byte {
	t.Helper()
	rc, err := OpenLayer(context.Background(), src, desc)
	if err != nil {
		t.Fatalf("OpenLayer: %v", err)
	}
	defer closeReader(t, rc)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

func closeReader(t *testing.T, rc io.Closer) {
	t.Helper()
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func TestOpenLayerUncompressed(t *testing.T) {
	f := newFixture(t)
	payload := []byte("plain uncompressed tar bytes")
	l, desc := openLayerRaw(t, f, payload)
	if got := readOpened(t, l, desc); !bytes.Equal(got, payload) {
		t.Fatalf("decompressed = %q, want %q", got, payload)
	}
}

func TestOpenLayerGzip(t *testing.T) {
	f := newFixture(t)
	payload := []byte("gzip-compressed tar bytes")
	l, desc := openLayerRaw(t, f, gzipBytes(t, payload))
	if got := readOpened(t, l, desc); !bytes.Equal(got, payload) {
		t.Fatalf("gzip decompressed = %q, want %q", got, payload)
	}
}

func TestOpenLayerZstd(t *testing.T) {
	f := newFixture(t)
	payload := []byte("zstd-compressed tar bytes")
	l, desc := openLayerRaw(t, f, zstdBytes(t, payload))
	if got := readOpened(t, l, desc); !bytes.Equal(got, payload) {
		t.Fatalf("zstd decompressed = %q, want %q", got, payload)
	}
}

// TestOpenLayerZstdTrailingSkippableFrame is an end-to-end check of a
// zstd:chunked-shaped layer: a zstd data frame followed by the trailing skippable
// frame that carries the per-file TOC. The decompressed output is just the data,
// while the digest is over the WHOLE blob (data + skippable frame), so the read
// must verify. (klauspost's decoder happens to consume the skippable frame itself;
// TestVerifyingLayerReaderDrainsTrailingBytes is the dedicated proof that the
// end-of-stream drain covers any bytes a decompressor leaves behind.)
func TestOpenLayerZstdTrailingSkippableFrame(t *testing.T) {
	f := newFixture(t)
	payload := []byte("zstd data before the skippable TOC frame")

	raw := zstdBytes(t, payload)
	// skippable frame: magic 0x184D2A50 (little-endian) + uint32 LE size + payload.
	toc := []byte("pretend-TOC-bytes")
	raw = append(raw, 0x50, 0x2A, 0x4D, 0x18)
	szLE := make([]byte, 4)
	binary.LittleEndian.PutUint32(szLE, uint32(len(toc))) // #nosec G115 -- test payload, fixed small length
	raw = append(raw, szLE...)
	raw = append(raw, toc...)

	l, desc := openLayerRaw(t, f, raw)
	if got := readOpened(t, l, desc); !bytes.Equal(got, payload) {
		t.Fatalf("decompressed = %q, want %q (skippable frame must not corrupt output)", got, payload)
	}
}

// TestOpenLayerDigestMismatchSurfacesAsReadError tampers the source blob (same
// length, so size passes); reading to EOF must return a verification error rather
// than a clean io.EOF, so a consumer cannot miss the corruption.
func TestOpenLayerDigestMismatchSurfacesAsReadError(t *testing.T) {
	f := newFixture(t)
	data := []byte("real uncompressed content")
	desc := f.writeBlob(data, ocispec.MediaTypeImageLayer)
	tampered := []byte("fake uncompressed content")
	if len(tampered) != len(data) {
		t.Fatalf("test setup: lengths differ")
	}
	f.setBlob(desc.Digest, tampered)

	rc, err := OpenLayer(context.Background(), f, desc)
	if err != nil {
		t.Fatalf("OpenLayer (open must succeed; verification is at EOF): %v", err)
	}
	defer closeReader(t, rc)
	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected a verification error reading a tampered layer to EOF")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
}

// TestOpenLayerPartialReadIsNotVerified pins the flip side of the OpenLayer
// contract: verification happens only at end of stream, so a consumer that
// stops reading before EOF and closes the layer observes no error even when
// the blob is tampered. A caller must consume the stream to EOF before
// trusting any bytes it read.
func TestOpenLayerPartialReadIsNotVerified(t *testing.T) {
	f := newFixture(t)
	data := []byte("real uncompressed content")
	desc := f.writeBlob(data, ocispec.MediaTypeImageLayer)
	tampered := []byte("fake uncompressed content")
	f.setBlob(desc.Digest, tampered)

	rc, err := OpenLayer(context.Background(), f, desc)
	if err != nil {
		t.Fatalf("OpenLayer: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close after partial read must not report verification: %v", err)
	}
}

func TestOpenLayerOversizedSourceRejected(t *testing.T) {
	f := newFixture(t)
	data := []byte("the real bytes")
	desc := f.writeBlob(data, ocispec.MediaTypeImageLayer)
	// Append extra bytes so the source exceeds desc.Size; the bounded read
	// (desc.Size+1) must catch it at EOF.
	f.setBlob(desc.Digest, append(data, []byte("EXTRA-OVERFLOW")...))

	rc, err := OpenLayer(context.Background(), f, desc)
	if err != nil {
		t.Fatalf("OpenLayer: %v", err)
	}
	defer closeReader(t, rc)
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "larger than descriptor") {
		t.Fatalf("error = %v, want oversize rejection", err)
	}
}

func TestOpenLayerUndersizedSourceRejected(t *testing.T) {
	f := newFixture(t)
	data := []byte("the real bytes")
	desc := f.writeBlob(data, ocispec.MediaTypeImageLayer)
	// Claim a larger size than the actual blob.
	desc.Size = int64(len(data)) + 100

	rc, err := OpenLayer(context.Background(), f, desc)
	if err != nil {
		t.Fatalf("OpenLayer: %v", err)
	}
	defer closeReader(t, rc)
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("error = %v, want size mismatch", err)
	}
}

// TestOpenLayerCtxCancelledBeforeOpen covers the entry guard.
func TestOpenLayerCtxCancelledBeforeOpen(t *testing.T) {
	f := newFixture(t)
	l, desc := openLayerRaw(t, f, []byte("content"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenLayer(ctx, l, desc); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled at open", err)
	}
}

// TestOpenLayerCtxCancelledDuringRead covers the streaming ctxReader: the layer
// opens with a live context that is then cancelled, and the read must fail with
// the cancellation. The payload exceeds bufio's buffer so a post-cancel Read
// genuinely reaches the source (a regression dropping the ctxReader would let the
// read complete from the already-buffered prefix and succeed).
func TestOpenLayerCtxCancelledDuringRead(t *testing.T) {
	f := newFixture(t)
	payload := bytes.Repeat([]byte("knarr-streaming-payload;"), 4096) // ~96 KiB > bufio buffer
	l, desc := openLayerRaw(t, f, payload)
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := OpenLayer(ctx, l, desc)
	if err != nil {
		t.Fatalf("OpenLayer: %v", err)
	}
	defer closeReader(t, rc)
	cancel()
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled during read", err)
	}
}

// TestVerifyingLayerReaderDrainsTrailingBytes proves the end-of-stream drain is
// load-bearing. It builds the reader with a stand-in decompressor that consumes
// only the first few compressed bytes before returning EOF; verification must
// still cover the WHOLE blob, which holds only if finish() drains the remainder
// through the hasher. Without the drain the byte count (and digest) would fall
// short and the read would fail.
func TestVerifyingLayerReaderDrainsTrailingBytes(t *testing.T) {
	src := bytes.Repeat([]byte("knarr-layer-bytes;"), 16)
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromBytes(src), Size: int64(len(src))}

	h := sha256.New()
	counter := &byteCounter{}
	teed := io.TeeReader(io.LimitReader(bytes.NewReader(src), desc.Size+1), io.MultiWriter(h, counter))
	br := bufio.NewReader(teed)

	lr := &verifyingLayerReader{
		desc:    desc,
		decomp:  io.LimitReader(br, 5), // "decompressor" stops after 5 bytes, leaving the rest
		drain:   br,
		hasher:  h,
		counter: counter,
	}

	got, err := io.ReadAll(lr)
	if err != nil {
		t.Fatalf("read: %v (the drain must let the whole blob verify)", err)
	}
	if !bytes.Equal(got, src[:5]) {
		t.Fatalf("emitted = %q, want the 5-byte prefix %q", got, src[:5])
	}
	if counter.n != int64(len(src)) {
		t.Fatalf("hashed %d bytes, want the whole blob (%d) — drain did not cover the tail", counter.n, len(src))
	}
}

// --- Malformed-digest path safety and post-auth tamper ---

// TestResolveRejectsPathEscapeManifestDigest pins an index whose (matching)
// manifest entry carries a digest crafted to escape blobs/sha256. The digest
// must be rejected as malformed before it is ever used to build a path.
func TestResolveRejectsPathEscapeManifestDigest(t *testing.T) {
	f := newFixture(t)
	bad := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.Digest("sha256:../../../../etc/passwd"),
		Size:      10,
		Platform:  &ocispec.Platform{OS: "linux", Architecture: "amd64"},
	}
	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{bad}), ocispec.MediaTypeImageIndex)

	_, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of a path-escape manifest digest")
	}
	if !strings.Contains(err.Error(), "invalid digest") {
		t.Fatalf("error = %v, want invalid-digest rejection", err)
	}
}

// TestResolveRejectsMalformedRootNoPanic feeds a digest with no algorithm
// separator. Algorithm() would panic on it, so this also guards that validation
// happens via Validate() (which does not panic) before any such use.
func TestResolveRejectsMalformedRootNoPanic(t *testing.T) {
	f := newFixture(t)
	_, err := Resolve(context.Background(), f, digest.Digest("deadbeefnocolon"),
		ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected error for a malformed (no-separator) root digest")
	}
}

// TestResolveRejectsNonSHA256Layer ensures a non-sha256 layer descriptor is
// rejected by Resolve itself, not deferred to the layer reader: the resolved
// chain must not expose a layer descriptor the caller cannot trust.
func TestResolveRejectsNonSHA256Layer(t *testing.T) {
	f := newFixture(t)
	badLayer := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerZstd,
		Digest:    digest.Digest("sha512:" + strings.Repeat("c", 128)),
		Size:      5,
	}
	layers := []ocispec.Descriptor{badLayer}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	_, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of a non-sha256 layer descriptor")
	}
	if !strings.Contains(err.Error(), "layer[0]") {
		t.Fatalf("error = %v, want layer descriptor rejection", err)
	}
}

// TestResolveTamperedManifestRejected tampers the manifest blob after the index
// (which references it) is authenticated, so the manifest digest no longer
// matches. Verification must catch it even though the root index was valid.
func TestResolveTamperedManifestRejected(t *testing.T) {
	f := newFixture(t)
	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
	man.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	idx := f.writeBlob(indexJSON([]ocispec.Descriptor{man}), ocispec.MediaTypeImageIndex)

	f.flipBlob(man.Digest)

	_, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of a tampered manifest blob")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
}

// TestResolveTamperedConfigRejected tampers the config blob of a bare manifest;
// the config digest no longer matches and must be rejected.
func TestResolveTamperedConfigRejected(t *testing.T) {
	f := newFixture(t)
	layers := []ocispec.Descriptor{fakeLayerDesc("l")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	f.flipBlob(cfg.Digest)

	_, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected rejection of a tampered config blob")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
}

// --- Platform matcher units ---

func TestPlatformEquals(t *testing.T) {
	tests := []struct {
		a, b     *ocispec.Platform
		expected bool
	}{
		{
			a:        &ocispec.Platform{OS: "linux", Architecture: "amd64"},
			b:        &ocispec.Platform{OS: "linux", Architecture: "amd64"},
			expected: true,
		},
		{
			a:        &ocispec.Platform{OS: "linux", Architecture: "arm64"},
			b:        &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
			expected: true, // arm64 default variant is v8
		},
		{
			a:        &ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v7"},
			b:        &ocispec.Platform{OS: "linux", Architecture: "arm64"},
			expected: false, // v7 != v8
		},
		{
			a:        &ocispec.Platform{OS: "linux", Architecture: "amd64"},
			b:        &ocispec.Platform{OS: "darwin", Architecture: "amd64"},
			expected: false,
		},
		{a: nil, b: nil, expected: true},
		{a: &ocispec.Platform{}, b: nil, expected: false},
	}
	for i, tt := range tests {
		if got := platformEquals(tt.a, tt.b); got != tt.expected {
			t.Errorf("test %d: platformEquals(%v, %v) = %v, want %v", i, tt.a, tt.b, got, tt.expected)
		}
	}
}

// trackingSource wraps a fixture so tests can assert every opened metadata
// blob reader is closed again.
type trackingSource struct {
	*fixture
	closers []*trackedCloser
}

type trackedCloser struct {
	io.ReadCloser
	closed bool
}

func (c *trackedCloser) Close() error { c.closed = true; return c.ReadCloser.Close() }

func (s *trackingSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := s.fixture.Open(ctx, desc)
	if err != nil {
		return nil, err
	}
	tc := &trackedCloser{ReadCloser: rc}
	s.closers = append(s.closers, tc)
	return tc, nil
}

// TestResolveClosesMetadataReaders pins that every metadata blob reader opened
// during Resolve is closed before it returns, so repeated resolves cannot leak
// file descriptors from a file-backed source.
func TestResolveClosesMetadataReaders(t *testing.T) {
	f := newFixture(t)
	layers := []ocispec.Descriptor{fakeLayerDesc("l1")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)

	src := &trackingSource{fixture: f}
	if _, err := Resolve(context.Background(), src, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(src.closers) == 0 {
		t.Fatal("tracking source saw no opens")
	}
	for i, c := range src.closers {
		if !c.closed {
			t.Fatalf("metadata reader %d was not closed", i)
		}
	}
}

// TestResolveClassifiesRootWithoutMediaType covers the structural fallback: a
// root blob whose JSON omits mediaType is classified by shape — manifest
// entries and no config mean an index; a config descriptor means a manifest.
func TestResolveClassifiesRootWithoutMediaType(t *testing.T) {
	t.Run("index", func(t *testing.T) {
		f := newFixture(t)
		layers := []ocispec.Descriptor{fakeLayerDesc("l1")}
		cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
		man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
		man.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
		idx := f.writeBlob(stripMediaType(t, indexJSON([]ocispec.Descriptor{man})), ocispec.MediaTypeImageIndex)

		res, err := Resolve(context.Background(), f, idx.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
		if err != nil {
			t.Fatalf("Resolve mediaType-less index: %v", err)
		}
		if res.ManifestDesc.Digest != man.Digest {
			t.Fatalf("selected manifest = %s, want %s", res.ManifestDesc.Digest, man.Digest)
		}
	})
	t.Run("manifest", func(t *testing.T) {
		f := newFixture(t)
		layers := []ocispec.Descriptor{fakeLayerDesc("l1")}
		cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
		man := f.writeBlob(stripMediaType(t, manifestJSON(cfg, layers)), ocispec.MediaTypeImageManifest)

		res, err := Resolve(context.Background(), f, man.Digest, ocispec.Platform{OS: "linux", Architecture: "amd64"})
		if err != nil {
			t.Fatalf("Resolve mediaType-less manifest: %v", err)
		}
		if res.ConfigDesc.Digest != cfg.Digest {
			t.Fatalf("config desc = %s, want %s", res.ConfigDesc.Digest, cfg.Digest)
		}
	})
}

func stripMediaType(t *testing.T, blob []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	delete(m, "mediaType")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
