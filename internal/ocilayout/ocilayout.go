// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ocilayout opens local OCI image layout blobs from a directory.
package ocilayout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/ocichain"
)

// Open opens the OCI image layout rooted at dir.
func Open(dir string) (ocichain.BlobSource, error) {
	if dir == "" {
		return nil, errors.New("ocilayout: empty layout directory")
	}
	return &source{dir: dir}, nil
}

// PlatformString returns the os/arch[/variant] string for a platform.
func PlatformString(p *ocispec.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ParsePlatform parses an os/arch[/variant] platform string.
func ParsePlatform(s string) (ocispec.Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return ocispec.Platform{}, fmt.Errorf("ocilayout: invalid platform %q", s)
	}
	for i, part := range parts {
		parts[i] = strings.ToLower(part)
		if !validPlatformComponent(parts[i]) {
			return ocispec.Platform{}, fmt.Errorf("ocilayout: invalid platform component %q", part)
		}
	}
	p := ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}

type (
	source struct {
		dir string
	}

	// rangeReader serves one section of an open blob file and fails a Read as
	// soon as ctx is cancelled, so a long copy is interrupted between reads.
	rangeReader struct {
		ctx     context.Context
		section *io.SectionReader
		f       *os.File
	}
)

// Open opens the raw bytes for desc from blobs/<algorithm>/<encoded>.
func (s *source) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	f, _, err := s.openBlob(ctx, desc)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// OpenRange serves n bytes of desc's blob file starting at off. The range must
// lie within the file; the answer is never the whole blob.
func (s *source) OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	if off < 0 || n < 0 || off > desc.Size || n > desc.Size-off {
		return nil, false, fmt.Errorf("ocilayout: range %d+%d exceeds blob %s of size %d", off, n, desc.Digest, desc.Size)
	}
	f, size, err := s.openBlob(ctx, desc)
	if err != nil {
		return nil, false, err
	}
	// The descriptor bounds the range; the file must hold it.
	if off > size || n > size-off {
		err := fmt.Errorf("ocilayout: blob %s file holds %d bytes, range %d+%d is past its end", desc.Digest, size, off, n)
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("ocilayout: close blob %s: %w", desc.Digest, closeErr))
		}
		return nil, false, err
	}
	return &rangeReader{ctx: ctx, section: io.NewSectionReader(f, off, n), f: f}, false, nil
}

// DescribeMissingRoot returns a diagnostic for a pinned root digest absent from
// the layout's blobs directory.
func (s *source) DescribeMissingRoot(root digest.Digest, cause error) error {
	f, err := os.Open(filepath.Join(s.dir, "index.json")) // #nosec G304 -- layout entrypoint
	if err != nil {
		return fmt.Errorf("ocilayout: root %s not present in the layout and index.json is unreadable: %w", root, errors.Join(cause, err))
	}
	defer warnClose("ocilayout: failed to close index.json", f)

	data, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	if err != nil || int64(len(data)) > maxMetadataBytes {
		return fmt.Errorf("ocilayout: root %s not present in the layout: %w", root, cause)
	}
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil || len(index.Manifests) == 0 {
		return fmt.Errorf("ocilayout: root %s not present in the layout: %w", root, cause)
	}
	refs := make([]string, 0, len(index.Manifests))
	for _, m := range index.Manifests {
		s := m.Digest.String()
		if m.Platform != nil {
			s += " (" + PlatformString(m.Platform) + ")"
		}
		refs = append(refs, s)
	}
	return fmt.Errorf("ocilayout: root %s is not present in the layout; its index.json references %s — pin a digest present in the layout, or re-export it preserving the image index",
		root, strings.Join(refs, ", "))
}

// openBlob opens desc's regular file under blobs/<algorithm>/<encoded> and
// returns it with its size.
func (s *source) openBlob(ctx context.Context, desc ocispec.Descriptor) (*os.File, int64, error) {
	if err := digestutil.ValidateSHA256(desc.Digest); err != nil {
		return nil, 0, fmt.Errorf("ocilayout: blob digest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	path := filepath.Join(s.dir, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Hex())
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 -- digest-derived blob path, verified by caller
	if err != nil {
		return nil, 0, fmt.Errorf("ocilayout: open blob %s: %w", desc.Digest, err)
	}

	fi, err := f.Stat()
	if err != nil {
		closeErr := f.Close()
		err = fmt.Errorf("ocilayout: stat blob %s: %w", desc.Digest, err)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("ocilayout: close blob %s: %w", desc.Digest, closeErr))
		}
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		closeErr := f.Close()
		err := fmt.Errorf("ocilayout: blob %s is not a regular file", desc.Digest)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("ocilayout: close blob %s: %w", desc.Digest, closeErr))
		}
		return nil, 0, err
	}

	return f, fi.Size(), nil
}

func (r *rangeReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.section.Read(p)
}

func (r *rangeReader) Close() error {
	return r.f.Close()
}

const maxMetadataBytes = 32 << 20 // 32 MiB

func validPlatformComponent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func warnClose(msg string, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(msg, "err", err)
	}
}
