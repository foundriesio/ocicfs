// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/distribution/reference"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/zstdchunked"
)

// Image is one resolved image: the reference it was named by, the platform
// manifest it resolved to, and its layers bottom-up.
type Image struct {
	Ref            string     `json:"ref"`
	Platform       string     `json:"platform"`
	IndexDigest    string     `json:"indexDigest,omitempty"` // empty for a bare manifest
	ManifestDigest string     `json:"manifestDigest"`
	ConfigDigest   string     `json:"configDigest"`
	IndexSize      int64      `json:"indexSize,omitempty"`
	ManifestSize   int64      `json:"manifestSize"`
	ConfigSize     int64      `json:"configSize"`
	Layers         []LayerRef `json:"layers"`
}

// LayerRef is one layer of an image. ChainID identifies the snapshot the
// layer and everything below it make.
type LayerRef struct {
	BlobDigest string `json:"blobDigest"`
	MediaType  string `json:"mediaType"`
	BlobSize   int64  `json:"blobSize"`
	DiffID     string `json:"diffID"`
	ChainID    string `json:"chainID"`
	// TOC and TOCChecksum are the zstd:chunked table of contents position
	// and checksum annotations, empty for a layer without them.
	TOC         string `json:"toc,omitempty"`
	TOCChecksum string `json:"tocChecksum,omitempty"`
}

// opener opens a registry reference as a blob source and returns its root
// digest.
type opener func(ctx context.Context, ref string) (ocichain.BlobSource, digest.Digest, error)

// readRefs expands the command-line references: @path reads one reference
// per line from a file (@- from stdin), skipping blank lines and # comments.
// No reference at all is a usage error.
func readRefs(args []string, stdin io.Reader) ([]string, error) {
	var refs []string
	for _, a := range args {
		file, ok := strings.CutPrefix(a, "@")
		if !ok {
			refs = append(refs, a)
			continue
		}
		listed, err := readRefFile(file, stdin)
		if err != nil {
			return nil, err
		}
		refs = append(refs, listed...)
	}
	if len(refs) == 0 {
		return nil, &cli.UsageError{Err: errors.New("no image references given")}
	}
	return refs, nil
}

func readRefFile(file string, stdin io.Reader) ([]string, error) {
	r := stdin
	if file != "-" {
		f, err := os.Open(file) // #nosec G304 -- a reference list the user names
		if err != nil {
			return nil, fmt.Errorf("read reference list: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	var refs []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		refs = append(refs, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read reference list %s: %w", file, err)
	}
	return refs, nil
}

// resolveImage resolves ref for platform through the spool and returns the
// image with its layers and ChainIDs, and the spooled source of its blobs.
func resolveImage(ctx context.Context, open opener, sp *spool, ref string, platform ocispec.Platform) (*Image, ocichain.BlobSource, error) {
	src, root, err := open(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	s := sp.source(src)
	res, err := ocichain.Resolve(ctx, s, root, platform)
	if err != nil {
		return nil, nil, platformError(ctx, s, root, platform, err)
	}
	sp.keepMeta(res.RootDesc.Digest, res.RootBytes)
	sp.keepMeta(res.ManifestDesc.Digest, res.ManifestBytes)
	sp.keepMeta(res.ConfigDesc.Digest, res.ConfigBytes)
	img := &Image{
		Ref:            normalizeRef(ref),
		Platform:       ocilayout.PlatformString(&res.Platform),
		ManifestDigest: res.ManifestDesc.Digest.String(),
		ManifestSize:   int64(len(res.ManifestBytes)),
		ConfigDigest:   res.ConfigDesc.Digest.String(),
		ConfigSize:     int64(len(res.ConfigBytes)),
	}
	if res.RootDesc.Digest != res.ManifestDesc.Digest {
		img.IndexDigest, img.IndexSize = res.RootDesc.Digest.String(), int64(len(res.RootBytes))
	}
	var chain digest.Digest
	for i, l := range res.Manifest.Layers {
		chain = chainID(chain, res.DiffIDs[i])
		img.Layers = append(img.Layers, LayerRef{
			BlobDigest:  l.Digest.String(),
			MediaType:   l.MediaType,
			BlobSize:    l.Size,
			DiffID:      res.DiffIDs[i].String(),
			ChainID:     chain.String(),
			TOC:         l.Annotations[zstdchunked.ManifestPositionKey],
			TOCChecksum: l.Annotations[zstdchunked.ManifestChecksumKey],
		})
	}
	return img, s, nil
}

// chainID extends the ChainID of the layers below with diffID.
func chainID(parent, diffID digest.Digest) digest.Digest {
	if parent == "" {
		return diffID
	}
	return digest.FromString(parent.String() + " " + diffID.String())
}

// platformError adds the platforms an index offers to a resolve failure.
func platformError(ctx context.Context, src ocichain.BlobSource, root digest.Digest, want ocispec.Platform, cause error) error {
	raw, err := ocichain.ReadRootBlob(ctx, src, root)
	if err != nil {
		return cause
	}
	if isIndex, err := ocichain.IsIndex(raw); err != nil || !isIndex {
		return cause
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return cause
	}
	var have []string
	for _, m := range index.Manifests {
		if m.Platform != nil {
			have = append(have, ocilayout.PlatformString(m.Platform))
		}
	}
	if _, err := ocichain.SelectManifestDesc(raw, want); err == nil {
		return cause
	}
	return fmt.Errorf("%w (the index offers: %s; pick one with --platform)", cause, strings.Join(have, ", "))
}

// spool holds every blob fetched during one run in a temporary directory,
// so a layer read twice is downloaded once. Manifests, configs and indexes
// are also kept in the cache directory, by digest, across runs. It counts
// what it downloads, whole blobs and ranges.
type spool struct {
	dir         string
	metaDir     string // "" when nothing is kept across runs
	fetched     map[digest.Digest]int64
	order       []digest.Digest
	inMemory    map[digest.Digest][]byte
	ranges      int
	rangedBytes int64
}

// newSpool creates the run's spool under a new temporary directory; close
// removes it.
func newSpool(metaDir string) (*spool, error) {
	dir, err := os.MkdirTemp("", "ocicfs-size-spool-*")
	if err != nil {
		return nil, fmt.Errorf("create spool: %w", err)
	}
	return &spool{dir: dir, metaDir: metaDir, fetched: make(map[digest.Digest]int64), inMemory: make(map[digest.Digest][]byte)}, nil
}

func (s *spool) close() {
	if err := os.RemoveAll(s.dir); err != nil {
		slog.Warn("remove spool", "dir", s.dir, "err", err)
	}
}

// source returns a blob source that serves src's blobs from the spool.
func (s *spool) source(src ocichain.BlobSource) ocichain.BlobSource {
	return spoolSource{s: s, src: src}
}

// keepMeta records verified metadata bytes so later runs need not fetch them.
func (s *spool) keepMeta(d digest.Digest, data []byte) {
	s.inMemory[d] = data
	if s.metaDir == "" {
		return
	}
	p := filepath.Join(s.metaDir, d.Encoded())
	if _, err := os.Stat(p); err == nil {
		return
	}
	if err := writeFileAtomic(p, data); err != nil {
		slog.Warn("cache metadata blob", "digest", d, "err", err)
	}
}

// open serves a blob from memory, the metadata cache or the spool, fetching
// it into the spool on a miss.
func (s *spool) open(ctx context.Context, src ocichain.BlobSource, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if err := desc.Digest.Validate(); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if data, ok := s.inMemory[desc.Digest]; ok {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	if s.metaDir != "" {
		if f, err := os.Open(filepath.Join(s.metaDir, desc.Digest.Encoded())); err == nil {
			return f, nil
		}
	}
	if _, ok := s.fetched[desc.Digest]; !ok {
		r, err := src.Open(ctx, desc)
		if err != nil {
			return nil, err
		}
		if err := s.keep(desc, r); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(s.path(desc)) // #nosec G304 -- spool-owned path named by a validated digest
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	return f, nil
}

// openRange serves n bytes of a layer blob from offset off: out of the spool
// when the blob is there, as a range of src when src serves ranges, and out
// of the whole blob, spooled, otherwise or when src answers with the whole
// blob.
func (s *spool) openRange(ctx context.Context, src ocichain.BlobSource, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, error) {
	if err := desc.Digest.Validate(); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	_, spooled := s.fetched[desc.Digest]
	if rs, ok := src.(ocichain.RangeSource); ok && !spooled {
		r, whole, err := rs.OpenRange(ctx, desc, off, n)
		if err != nil {
			return nil, err
		}
		if !whole {
			s.ranges++
			s.rangedBytes += n
			return r, nil
		}
		if err := s.keep(desc, r); err != nil {
			return nil, err
		}
	} else if !spooled {
		r, err := src.Open(ctx, desc)
		if err != nil {
			return nil, err
		}
		if err := s.keep(desc, r); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(s.path(desc)) // #nosec G304 -- spool-owned path named by a validated digest
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, off, n), f}, nil
}

func (s *spool) path(desc ocispec.Descriptor) string {
	return filepath.Join(s.dir, desc.Digest.Encoded())
}

// keep copies the blob r streams into the spool, closing r, and counts it.
func (s *spool) keep(desc ocispec.Descriptor, r io.ReadCloser) error {
	p := s.path(desc)
	f, err := os.Create(p) // #nosec G304 -- spool-owned path named by a validated digest
	if err != nil {
		return errors.Join(fmt.Errorf("spool: %w", err), r.Close())
	}
	n, err := io.Copy(f, r)
	err = errors.Join(err, r.Close(), f.Close())
	if err != nil {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			slog.Warn("remove partial spool file", "path", p, "err", rerr)
		}
		return fmt.Errorf("spool: fetch %s: %w", desc.Digest, err)
	}
	s.fetched[desc.Digest] = n
	s.order = append(s.order, desc.Digest)
	return nil
}

// spoolSource is one registry source seen through the spool.
type spoolSource struct {
	s   *spool
	src ocichain.BlobSource
}

func (ss spoolSource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	return ss.s.open(ctx, ss.src, desc)
}

// OpenRange serves a range through the spool; it never answers with the
// whole blob.
func (ss spoolSource) OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	r, err := ss.s.openRange(ctx, ss.src, desc, off, n)
	return r, false, err
}

// writeFileAtomic writes data to p through a temporary file and a rename,
// creating p's directory.
func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			slog.Warn("remove temporary file", "path", tmp, "err", rerr)
		}
		return err
	}
	return nil
}

// normalizeRef is ref as a fully qualified name with its tag, or with its
// digest when it names one; a ref that does not parse as a name is kept as
// given.
func normalizeRef(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	if _, ok := named.(reference.Digested); ok {
		return named.String()
	}
	return reference.TagNameOnly(named).String()
}
