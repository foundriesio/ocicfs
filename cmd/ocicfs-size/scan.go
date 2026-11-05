// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
)

// LayerInv is the inventory of one layer: every tar entry in stream order,
// normalised, with each regular file's content hashed. It depends on the
// layer's uncompressed tar alone, so it is cached by DiffID.
type LayerInv struct {
	Schema      int      `json:"schema"`
	DiffID      string   `json:"diffID"`
	TarSize     int64    `json:"tarSize"`
	Entries     []Entry  `json:"entries"`
	Notes       []string `json:"notes,omitempty"`
	Unsupported string   `json:"unsupported,omitempty"` // why the layer-scan models cannot size this layer
}

// Entry is one tar entry. Path is absolute and clean; the root is "/". A
// whiteout names the path it removes, an opaque marker its directory.
type Entry struct {
	Path      string  `json:"p"`
	Kind      Kind    `json:"k"`
	Mode      uint32  `json:"m,omitempty"` // permission and special bits
	UID       uint32  `json:"u,omitempty"`
	GID       uint32  `json:"g,omitempty"`
	MtimeSec  int64   `json:"t,omitempty"`
	MtimeNsec uint32  `json:"n,omitempty"`
	Size      int64   `json:"s,omitempty"` // content bytes of a regular file
	Digest    string  `json:"d,omitempty"` // hex sha256 of a regular file's content
	Link      int32   `json:"l"`           // a hardlink's target: the index of a regular file of this layer, or -1
	LinkName  string  `json:"ln,omitempty"`
	Devmajor  int64   `json:"dM,omitempty"`
	Devminor  int64   `json:"dm,omitempty"`
	Xattrs    []Xattr `json:"x,omitempty"`
	Sparse    bool    `json:"sp,omitempty"`
	ZSize     int64   `json:"z,omitempty"` // a non-empty regular file's content compressed alone at zstd level 10, when the scan records it
}

// Xattr is one extended attribute of an entry.
type Xattr struct {
	Name  string `json:"k"`
	Value string `json:"v"`
}

// Kind is the type of a tar entry.
type Kind uint8

// The entry kinds.
const (
	KindReg Kind = iota
	KindDir
	KindSymlink
	KindHardlink
	KindChar
	KindBlock
	KindFIFO
	KindWhiteout
	KindOpaque
)

// The scan's bounds and the version of its rules.
const (
	// scanSchema changes whenever the scan records a layer differently, so an
	// inventory cached under other rules is never read back.
	scanSchema      = 2
	maxLayerEntries = 4 << 20
	maxXattrValue   = 64 << 10
)

// scanLayer reads a decompressed layer tar from r and returns its inventory,
// after reading the stream to its end, closing it, and checking it hashes to
// diffID. A failure at any of those steps returns no inventory. With
// zsizes it records every non-empty regular file's compressed size too.
func scanLayer(ctx context.Context, r io.ReadCloser, diffID digest.Digest, zsizes bool) (*LayerInv, error) {
	inv, err := scanTar(ctx, r, diffID, zsizes)
	if cerr := r.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("scan layer %s: close: %w", diffID, cerr)
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// scanTar walks the tar stream, drains it to the decompressor's end and
// verifies the diff_id.
func scanTar(ctx context.Context, r io.Reader, diffID digest.Digest, zsizes bool) (*LayerInv, error) {
	var enc *zstd.Encoder
	if zsizes {
		var err error
		// The level and options containers/storage writes zstd:chunked
		// payloads with.
		if enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(10))); err != nil {
			return nil, fmt.Errorf("scan layer %s: %w", diffID, err)
		}
	}
	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(r, h)}
	tr := tar.NewReader(counted)
	inv := &LayerInv{Schema: scanSchema, DiffID: diffID.String()}
	byPath := make(map[string]int32)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scan layer %s: %w", diffID, err)
		}
		if len(inv.Entries) == maxLayerEntries {
			return nil, fmt.Errorf("scan layer %s: more than %d entries", diffID, maxLayerEntries)
		}
		e, keep, err := entryFromHeader(tr, hdr, inv, byPath, enc)
		if err != nil {
			return nil, fmt.Errorf("scan layer %s: %w", diffID, err)
		}
		if !keep {
			continue
		}
		if e.Kind != KindWhiteout && e.Kind != KindOpaque {
			byPath[e.Path] = int32(len(inv.Entries)) // #nosec G115 -- bounded by maxLayerEntries
		}
		inv.Entries = append(inv.Entries, e)
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return nil, fmt.Errorf("scan layer %s: drain: %w", diffID, err)
	}
	if got := digest.NewDigest(digest.SHA256, h); got != diffID {
		return nil, fmt.Errorf("scan layer: diff_id mismatch: expected %s, got %s", diffID, got)
	}
	inv.TarSize = counted.n
	return inv, nil
}

// entryFromHeader converts one tar header, reading a regular file's content
// to hash it, and to compress it alone with enc when enc is not nil. keep
// is false for an entry the layer does not record.
func entryFromHeader(tr *tar.Reader, hdr *tar.Header, inv *LayerInv, byPath map[string]int32, enc *zstd.Encoder) (Entry, bool, error) {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return Entry{}, false, nil
	}
	p, err := cleanPath(hdr.Name)
	if err != nil {
		return Entry{}, false, err
	}
	e := Entry{
		Path:      p,
		Mode:      uint32(hdr.Mode & 0o7777), // #nosec G115 -- masked to 12 bits
		UID:       clampID(hdr.Uid),
		GID:       clampID(hdr.Gid),
		MtimeSec:  hdr.ModTime.Unix(),
		MtimeNsec: uint32(hdr.ModTime.Nanosecond()), // #nosec G115 -- nanoseconds are below 1e9
		Link:      -1,
	}
	dir, base := path.Split(p)
	if strings.HasPrefix(base, ".wh.") {
		return whiteoutEntry(e, path.Clean(dir), base, inv)
	}
	if e.Xattrs, err = paxXattrs(hdr); err != nil {
		return Entry{}, false, fmt.Errorf("%s: %w", p, err)
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeGNUSparse:
		e.Kind = KindReg
		e.Sparse = hdr.Typeflag == tar.TypeGNUSparse || hdr.PAXRecords["GNU.sparse.major"] != "" || hdr.PAXRecords["GNU.sparse.size"] != ""
		if e.Sparse {
			inv.Notes = appendNote(inv.Notes, "sparse files are counted as dense")
		}
		h := sha256.New()
		var w io.Writer = h
		var z countingWriter
		if enc != nil {
			enc.Reset(&z)
			w = io.MultiWriter(h, enc)
		}
		n, err := io.Copy(w, tr)
		if err != nil {
			return Entry{}, false, fmt.Errorf("%s: read content: %w", p, err)
		}
		e.Size, e.Digest = n, hex.EncodeToString(h.Sum(nil))
		if enc != nil && n > 0 {
			if err := enc.Close(); err != nil {
				return Entry{}, false, fmt.Errorf("%s: compress content: %w", p, err)
			}
			e.ZSize = z.n
		}
	case tar.TypeDir:
		e.Kind = KindDir
	case tar.TypeSymlink:
		e.Kind, e.LinkName = KindSymlink, hdr.Linkname
	case tar.TypeLink:
		e.Kind = KindHardlink
		target, err := cleanPath(hdr.Linkname)
		if err != nil {
			return Entry{}, false, fmt.Errorf("%s: hardlink target: %w", p, err)
		}
		e.LinkName = target
		i, ok := byPath[target]
		if ok && inv.Entries[i].Kind == KindHardlink {
			i = inv.Entries[i].Link
		}
		switch {
		case ok && i >= 0 && linkable(inv.Entries[i].Kind):
			e.Link = i
		case !ok:
			if inv.Unsupported == "" {
				inv.Unsupported = "cross-layer hardlink " + p + " -> " + target
			}
		default:
			if inv.Unsupported == "" {
				inv.Unsupported = "hardlink to a directory " + p + " -> " + target
			}
		}
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		e.Kind = map[byte]Kind{tar.TypeChar: KindChar, tar.TypeBlock: KindBlock, tar.TypeFifo: KindFIFO}[hdr.Typeflag]
		e.Devmajor, e.Devminor = hdr.Devmajor, hdr.Devminor
		if hdr.Devmajor < 0 || hdr.Devmajor > math.MaxUint32 || hdr.Devminor < 0 || hdr.Devminor > math.MaxUint32 {
			if inv.Unsupported == "" {
				inv.Unsupported = fmt.Sprintf("device number %d:%d of %s out of range", hdr.Devmajor, hdr.Devminor, p)
			}
		}
	default:
		return Entry{}, false, fmt.Errorf("%s: unsupported tar entry type %q", p, hdr.Typeflag)
	}
	return e, true, nil
}

// linkable reports whether a hardlink may name an inode of kind k: any
// non-directory, as link(2) allows for symlinks, devices and FIFOs too.
func linkable(k Kind) bool {
	switch k {
	case KindReg, KindSymlink, KindChar, KindBlock, KindFIFO:
		return true
	}
	return false
}

// whiteoutEntry converts a .wh.-prefixed entry in dir: .wh..wh..opq marks dir
// opaque, .wh.<name> removes <name>, any other .wh..wh. name is ignored, and a
// bare .wh. is an error.
func whiteoutEntry(e Entry, dir, base string, inv *LayerInv) (Entry, bool, error) {
	switch {
	case base == ".wh..wh..opq":
		e.Kind, e.Path = KindOpaque, dir
	case base == ".wh.":
		return Entry{}, false, fmt.Errorf("%s: whiteout names nothing", e.Path)
	case strings.HasPrefix(base, ".wh..wh."):
		inv.Notes = appendNote(inv.Notes, "ignored whiteout metadata entries")
		return Entry{}, false, nil
	default:
		e.Kind, e.Path = KindWhiteout, path.Join(dir, strings.TrimPrefix(base, ".wh."))
	}
	e.Mode, e.UID, e.GID, e.MtimeSec, e.MtimeNsec = 0, 0, 0, 0, 0
	return e, true, nil
}

// cleanPath turns a tar name into an absolute clean path, refusing one that
// climbs out of the root.
func cleanPath(name string) (string, error) {
	rel := path.Clean(strings.TrimLeft(name, "/"))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("entry %q escapes the root", name)
	}
	return path.Clean("/" + rel), nil
}

// paxXattrs returns a header's SCHILY.xattr records in name order.
func paxXattrs(hdr *tar.Header) ([]Xattr, error) {
	var out []Xattr
	for k, v := range hdr.PAXRecords {
		name, ok := strings.CutPrefix(k, "SCHILY.xattr.")
		if !ok {
			continue
		}
		if len(v) > maxXattrValue {
			return nil, fmt.Errorf("extended attribute %q of %d bytes exceeds %d", name, len(v), maxXattrValue)
		}
		out = append(out, Xattr{Name: name, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// appendNote adds note unless notes already has it.
func appendNote(notes []string, note string) []string {
	for _, n := range notes {
		if n == note {
			return notes
		}
	}
	return append(notes, note)
}

// clampID keeps a negative tar uid or gid from wrapping around.
func clampID(id int) uint32 {
	if id < 0 || id > math.MaxUint32 {
		return 0
	}
	return uint32(id)
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// countingWriter counts the bytes written to it.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
