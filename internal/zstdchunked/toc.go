// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package zstdchunked

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
)

const (
	// Entry types, as the TOC spells them.
	TypeReg      = "reg"
	TypeHardlink = "hardlink"
	TypeSymlink  = "symlink"
	TypeDir      = "dir"
	TypeChar     = "char"
	TypeBlock    = "block"
	TypeFifo     = "fifo"

	typeChunk        = "chunk"
	decoderMaxMemory = 64 << 20
	// maxEntries caps the entries a TOC may keep. Entries are far larger in
	// memory than the few bytes of JSON they can be written in, so the byte
	// cap alone does not bound memory. A TOC of ordinary entries reaches the
	// byte cap first.
	maxEntries = 1 << 18
)

// TOC is a validated zstd:chunked table of contents in layer order.
type TOC struct {
	Entries []Entry
}

// Entry describes one layer entry and, for a regular file, where its compressed bytes are in the layer blob.
type Entry struct {
	// Type is one of the seven Type* constants.
	Type string
	// Name is the raw name from the tar. It is unsanitized: a caller resolving
	// it against a directory must reject absolute and parent-relative names.
	Name     string
	Linkname string
	Mode     int64
	Size     int64
	UID      int
	GID      int
	// ModTime is nil when the TOC omits it.
	ModTime  *time.Time
	Devmajor int64
	Devminor int64
	// Xattrs has keys without the SCHILY.xattr. prefix and values decoded to raw bytes.
	Xattrs map[string]string
	// Digest is the whole-file sha256, or empty for empty and non-regular entries.
	Digest digest.Digest
	// Offset and EndOffset are the byte range of the layer blob that holds the
	// file's compressed content: blob[Offset:EndOffset]. Both are zero for an
	// entry with no content.
	Offset    int64
	EndOffset int64
}

// Parse authenticates, decodes, and validates a compressed TOC. pos must come
// from Position, which bounds it against the blob size that Parse never sees.
func Parse(compressed []byte, pos Pos) (*TOC, error) {
	if pos.Offset < frameHeaderSize {
		return nil, fmt.Errorf("zstdchunked: manifest offset %d precedes its frame header", pos.Offset)
	}
	if pos.CompressedLen < 0 || pos.CompressedLen > maxTOCBytes {
		return nil, fmt.Errorf("zstdchunked: invalid compressed manifest length %d", pos.CompressedLen)
	}
	if int64(len(compressed)) != pos.CompressedLen {
		return nil, fmt.Errorf("zstdchunked: compressed manifest length %d does not match %d", len(compressed), pos.CompressedLen)
	}
	if err := digestutil.ValidateSHA256(pos.Checksum); err != nil {
		return nil, fmt.Errorf("zstdchunked: invalid manifest checksum: %w", err)
	}
	if got := digest.FromBytes(compressed); got != pos.Checksum {
		return nil, fmt.Errorf("zstdchunked: manifest checksum mismatch: got %s, want %s", got, pos.Checksum)
	}
	if pos.UncompressedLen < 0 || pos.UncompressedLen > maxTOCBytes {
		return nil, fmt.Errorf("zstdchunked: invalid uncompressed manifest length %d", pos.UncompressedLen)
	}

	decoder, err := zstd.NewReader(
		bytes.NewReader(compressed),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(decoderMaxMemory),
		zstd.WithDecodeBuffersBelow(0),
	)
	if err != nil {
		return nil, fmt.Errorf("zstdchunked: create manifest decoder: %w", err)
	}
	defer decoder.Close()

	// Read one byte past the annotated length so a longer manifest fails the
	// exact-length check below instead of being silently cut to size.
	counted := &countingReader{r: io.LimitReader(decoder, pos.UncompressedLen+1)}
	toc, err := decodeTOC(json.NewDecoder(counted), pos.Offset-frameHeaderSize)
	if err != nil {
		return nil, err
	}
	if counted.n != pos.UncompressedLen {
		return nil, fmt.Errorf("zstdchunked: uncompressed manifest length %d does not match %d", counted.n, pos.UncompressedLen)
	}
	return toc, nil
}

type (
	wireEntry struct {
		Type       string            `json:"type"`
		Name       string            `json:"name"`
		Linkname   string            `json:"linkName,omitempty"`
		Mode       int64             `json:"mode,omitempty"`
		Size       int64             `json:"size,omitempty"`
		UID        int               `json:"uid,omitempty"`
		GID        int               `json:"gid,omitempty"`
		ModTime    *time.Time        `json:"modtime,omitempty"`
		AccessTime *time.Time        `json:"accesstime,omitempty"`
		ChangeTime *time.Time        `json:"changetime,omitempty"`
		Devmajor   int64             `json:"devMajor,omitempty"`
		Devminor   int64             `json:"devMinor,omitempty"`
		Xattrs     map[string]string `json:"xattrs,omitempty"`
		Digest     string            `json:"digest,omitempty"`
		Offset     int64             `json:"offset,omitempty"`
		EndOffset  int64             `json:"endOffset,omitempty"`
	}

	byteRange struct {
		start int64
		end   int64
	}
)

// decodeTOC decodes the manifest's entries one at a time, so it never builds a
// record for every element of the document. The JSON decoder still buffers a
// run of whitespace or a single value whole, which the document size cap
// bounds.
func decodeTOC(dec *json.Decoder, payloadEnd int64) (*TOC, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	var entries []Entry
	seenVersion, seenEntries := false, false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("zstdchunked: decode manifest JSON: %w", err)
		}
		switch key {
		case "version":
			var version int
			if err := dec.Decode(&version); err != nil {
				return nil, fmt.Errorf("zstdchunked: decode manifest version: %w", err)
			}
			if seenVersion {
				return nil, fmt.Errorf("zstdchunked: manifest repeats its version")
			}
			if version != 1 {
				return nil, fmt.Errorf("zstdchunked: unsupported manifest version %d", version)
			}
			seenVersion = true
		case "entries":
			if seenEntries {
				return nil, fmt.Errorf("zstdchunked: manifest repeats its entries")
			}
			seenEntries = true
			if entries, err = decodeEntries(dec, payloadEnd); err != nil {
				return nil, err
			}
		default:
			var skipped json.RawMessage
			if err := dec.Decode(&skipped); err != nil {
				return nil, fmt.Errorf("zstdchunked: decode manifest JSON: %w", err)
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("zstdchunked: trailing data after manifest JSON")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("zstdchunked: after manifest JSON: %w", err)
	}
	if !seenVersion {
		return nil, fmt.Errorf("zstdchunked: manifest has no version")
	}
	if err := validateRanges(entries); err != nil {
		return nil, err
	}
	return &TOC{Entries: entries}, nil
}

func decodeEntries(dec *json.Decoder, payloadEnd int64) ([]Entry, error) {
	start, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("zstdchunked: decode manifest entries: %w", err)
	}
	// The writer marshals a layer without entries as null.
	if start == nil {
		return nil, nil
	}
	if start != json.Delim('[') {
		return nil, fmt.Errorf("zstdchunked: manifest entries are not an array")
	}
	var entries []Entry
	for i := 0; dec.More(); i++ {
		// A fresh value each time: Decode leaves fields an object omits as
		// they were and merges into an existing map.
		var wire wireEntry
		if err := dec.Decode(&wire); err != nil {
			return nil, fmt.Errorf("zstdchunked: decode entry %d: %w", i, err)
		}
		// A file is fetched as one byte range, so its chunk records are unused.
		if wire.Type == typeChunk {
			continue
		}
		if len(entries) == maxEntries {
			return nil, fmt.Errorf("zstdchunked: manifest exceeds %d entries", maxEntries)
		}
		if err := validateWireEntry(&wire); err != nil {
			return nil, fmt.Errorf("zstdchunked: entry %d %q: %w", i, wire.Name, err)
		}
		entry, err := convertEntry(&wire, payloadEnd)
		if err != nil {
			return nil, fmt.Errorf("zstdchunked: entry %d %q: %w", i, wire.Name, err)
		}
		entries = append(entries, entry)
	}
	if err := expectDelim(dec, ']'); err != nil {
		return nil, err
	}
	return entries, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	got, err := dec.Token()
	if err != nil {
		return fmt.Errorf("zstdchunked: decode manifest JSON: %w", err)
	}
	if got != want {
		return fmt.Errorf("zstdchunked: manifest JSON has %v where %v is expected", got, want)
	}
	return nil
}

func convertEntry(wire *wireEntry, payloadEnd int64) (Entry, error) {
	if !validEntryType(wire.Type) {
		return Entry{}, fmt.Errorf("unknown entry type %q", wire.Type)
	}
	if wire.Offset > wire.EndOffset {
		return Entry{}, fmt.Errorf("reversed byte range %d:%d", wire.Offset, wire.EndOffset)
	}
	xattrs, err := decodeXattrs(wire.Xattrs)
	if err != nil {
		return Entry{}, err
	}
	var d digest.Digest
	if wire.Type == TypeReg && wire.Size > 0 {
		d, err = parseDigest(wire.Digest)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid digest: %w", err)
		}
		if d == "" {
			return Entry{}, fmt.Errorf("non-empty regular file has no digest")
		}
		if wire.Offset >= wire.EndOffset {
			return Entry{}, fmt.Errorf("non-empty regular file has empty or reversed byte range %d:%d", wire.Offset, wire.EndOffset)
		}
		if wire.EndOffset > payloadEnd {
			return Entry{}, fmt.Errorf("regular file byte range ends past manifest frame at %d", payloadEnd)
		}
	} else if wire.Offset != 0 || wire.EndOffset != 0 {
		return Entry{}, fmt.Errorf("entry without payload names byte range %d:%d", wire.Offset, wire.EndOffset)
	}

	return Entry{
		Type:      wire.Type,
		Name:      wire.Name,
		Linkname:  wire.Linkname,
		Mode:      wire.Mode,
		Size:      wire.Size,
		UID:       wire.UID,
		GID:       wire.GID,
		ModTime:   wire.ModTime,
		Devmajor:  wire.Devmajor,
		Devminor:  wire.Devminor,
		Xattrs:    xattrs,
		Digest:    d,
		Offset:    wire.Offset,
		EndOffset: wire.EndOffset,
	}, nil
}

func validateWireEntry(entry *wireEntry) error {
	if entry.Size < 0 || entry.Offset < 0 || entry.EndOffset < 0 || entry.Mode < 0 ||
		entry.UID < 0 || entry.GID < 0 || entry.Devmajor < 0 || entry.Devminor < 0 {
		return fmt.Errorf("negative numeric field")
	}
	return nil
}

func validEntryType(typ string) bool {
	switch typ {
	case TypeReg, TypeHardlink, TypeSymlink, TypeDir, TypeChar, TypeBlock, TypeFifo:
		return true
	default:
		return false
	}
}

func decodeXattrs(encoded map[string]string) (map[string]string, error) {
	if encoded == nil {
		return nil, nil
	}
	decoded := make(map[string]string, len(encoded))
	for key, value := range encoded {
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode xattr %q: %w", key, err)
		}
		decoded[key] = string(data)
	}
	return decoded, nil
}

func parseDigest(value string) (digest.Digest, error) {
	if value == "" {
		return "", nil
	}
	d := digest.Digest(value)
	if err := digestutil.ValidateSHA256(d); err != nil {
		return "", err
	}
	return d, nil
}

func validateRanges(entries []Entry) error {
	ranges := make([]byteRange, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == TypeReg && entry.Size > 0 {
			ranges = append(ranges, byteRange{start: entry.Offset, end: entry.EndOffset})
		}
	}
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].start < ranges[j].start
	})
	for i := 1; i < len(ranges); i++ {
		if ranges[i].start < ranges[i-1].end {
			return fmt.Errorf("zstdchunked: regular file byte ranges overlap at %d", ranges[i].start)
		}
	}
	return nil
}

// countingReader counts the bytes read through it and fills every buffer it
// is given. A zstd stream can be cut into one-byte blocks, which the decoder
// hands out one per Read, and json.Decoder rescans the whitespace it has
// buffered after each Read: short reads would make a run of whitespace cost
// quadratic time.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	filled := 0
	for filled < len(p) {
		n, err := c.r.Read(p[filled:])
		filled += n
		c.n += int64(n)
		if err != nil {
			return filled, err
		}
	}
	return filled, nil
}
