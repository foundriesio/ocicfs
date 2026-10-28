// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocilayout

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/digestutil"
)

// Tar serves the blobs of an uncompressed tar of an OCI image layout, as
// docker save writes one, straight from the archive: nothing is extracted. A
// walk of the archive's headers at open time records where the bytes of
// index.json, manifest.json and every blobs/sha256/<hex> regular file sit, and
// each blob is then read as a section of the open file. Every other entry —
// directories, links, devices, sparse files, oci-layout, names outside the
// layout — is ignored. Close releases the archive once the last reader is
// done with it.
type Tar struct {
	f       *os.File
	entries map[string]tarEntry // cleaned entry name -> where its bytes sit
}

// tarEntry is the position of one archive entry's data.
type tarEntry struct {
	off, size int64
}

// OpenTar opens the layout archive at p and indexes its entries. A compressed
// archive is refused: docker save writes an uncompressed tar, and only that
// is seekable.
func OpenTar(p string) (*Tar, error) {
	f, err := os.Open(p) // #nosec G304 -- archive path given by the caller
	if err != nil {
		return nil, fmt.Errorf("ocilayout: open archive: %w", err)
	}
	entries, err := indexTar(f, p)
	if err != nil {
		warnClose("ocilayout: failed to close archive", f)
		return nil, err
	}
	return &Tar{f: f, entries: entries}, nil
}

// Open serves the raw bytes of desc's blobs/sha256/<hex> entry. The entry is
// served as it is in the archive; its digest and size are checked by the
// verifying reader every pull wraps around a source.
func (t *Tar) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	e, err := t.blob(ctx, desc)
	if err != nil {
		return nil, err
	}
	return &rangeReader{ctx: ctx, section: io.NewSectionReader(t.f, e.off, e.size)}, nil
}

// OpenRange serves n bytes of desc's blob entry starting at off. The range
// must lie within the entry; the answer is never the whole blob.
func (t *Tar) OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	if off < 0 || n < 0 || off > desc.Size || n > desc.Size-off {
		return nil, false, fmt.Errorf("ocilayout: range %d+%d exceeds blob %s of size %d", off, n, desc.Digest, desc.Size)
	}
	e, err := t.blob(ctx, desc)
	if err != nil {
		return nil, false, err
	}
	// The descriptor bounds the range; the entry must hold it.
	if off > e.size || n > e.size-off {
		return nil, false, fmt.Errorf("ocilayout: blob %s archive entry holds %d bytes, range %d+%d is past its end", desc.Digest, e.size, off, n)
	}
	return &rangeReader{ctx: ctx, section: io.NewSectionReader(t.f, e.off+off, n)}, false, nil
}

// DescribeMissingRoot returns a diagnostic for a pinned root digest absent from
// the archive's blobs.
func (t *Tar) DescribeMissingRoot(root digest.Digest, cause error) error {
	index, err := t.IndexJSON()
	if err != nil {
		return fmt.Errorf("ocilayout: root %s not present in the layout: %w", root, errors.Join(cause, err))
	}
	return describeMissingRoot(root, cause, index)
}

// IndexJSON returns the bytes of the archive's index.json entry.
func (t *Tar) IndexJSON() ([]byte, error) {
	e, ok := t.entries["index.json"]
	if !ok {
		return nil, errors.New("ocilayout: not a docker save / OCI layout archive: no index.json")
	}
	if e.size > maxMetadataBytes {
		return nil, fmt.Errorf("ocilayout: index.json is %d bytes, over the %d-byte bound", e.size, maxMetadataBytes)
	}
	data := make([]byte, e.size)
	if _, err := t.f.ReadAt(data, e.off); err != nil {
		return nil, fmt.Errorf("ocilayout: read index.json: %w", err)
	}
	return data, nil
}

// Close closes the archive.
func (t *Tar) Close() error {
	return t.f.Close()
}

// blob looks up desc's entry, validating the digest before it becomes a name.
func (t *Tar) blob(ctx context.Context, desc ocispec.Descriptor) (tarEntry, error) {
	if err := digestutil.ValidateSHA256(desc.Digest); err != nil {
		return tarEntry{}, fmt.Errorf("ocilayout: blob digest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return tarEntry{}, err
	}
	e, ok := t.entries["blobs/sha256/"+desc.Digest.Hex()]
	if !ok {
		return tarEntry{}, fmt.Errorf("ocilayout: open blob %s: %w", desc.Digest, os.ErrNotExist)
	}
	return e, nil
}

// indexTar walks the headers of the archive open on f and records the data
// position of every regular-file entry whose cleaned name is a layout name.
// The reader runs directly over the file, so an entry's data is skipped by
// seeking and, right after a header is read, the file position is where that
// entry's data starts. A later entry with the same name replaces an earlier
// one, as it does on extraction.
func indexTar(f *os.File, p string) (map[string]tarEntry, error) {
	if kind := compressionKind(f); kind != "" {
		return nil, fmt.Errorf("ocilayout: %s is a %s-compressed archive; decompress it first (docker save writes an uncompressed tar)", p, kind)
	}
	entries := make(map[string]tarEntry)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if errors.Is(err, tar.ErrInsecurePath) {
			// The header is still returned, but a name that is not local
			// cannot be a layout name.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("ocilayout: read archive %s: %w", p, err)
		}
		if hdr.Typeflag != tar.TypeReg || isSparse(hdr) {
			continue
		}
		name := path.Clean(hdr.Name)
		if !isLayoutEntryName(name) {
			continue
		}
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("ocilayout: read archive %s: %w", p, err)
		}
		entries[name] = tarEntry{off: off, size: hdr.Size}
	}
}

// isSparse reports whether a PAX sparse map describes hdr: the reader then
// reports the file's logical size while the archive holds only its data
// fragments, so the entry cannot be served as a section of the file.
func isSparse(hdr *tar.Header) bool {
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// isLayoutEntryName reports whether a cleaned entry name is one the archive
// is read for: index.json, manifest.json, or a well-formed sha256 blob path.
func isLayoutEntryName(name string) bool {
	if name == "index.json" || name == "manifest.json" {
		return true
	}
	hex, ok := strings.CutPrefix(name, "blobs/sha256/")
	return ok && digestutil.ValidateSHA256(digest.Digest("sha256:"+hex)) == nil
}

// compressionKind names the compression of the file open on f from its magic
// bytes — gzip, zstd, xz or bzip2 — or returns "" for anything else. The
// file position is left at the start.
func compressionKind(f *os.File) string {
	var head [6]byte
	n, _ := f.ReadAt(head[:], 0)
	for _, m := range []struct {
		kind  string
		magic []byte
	}{
		{"gzip", []byte{0x1f, 0x8b}},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}},
		{"bzip2", []byte("BZh")},
	} {
		if bytes.HasPrefix(head[:n], m.magic) {
			return m.kind
		}
	}
	return ""
}
