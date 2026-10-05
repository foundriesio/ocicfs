// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/digestutil"
)

// The format of a saved layer and the bounds it is written and read within.
const (
	// savedVersion is the format EncodeLayer writes and the only one
	// DecodeLayer accepts. It changes with the encoding, and whenever a scan
	// comes to record a layer differently: a saved layer stands in for a scan
	// of the same blob, so one written under other rules must not be read
	// back.
	savedVersion = 1
	// savedMaxBytes caps a saved layer, compressed and uncompressed alike.
	savedMaxBytes = 64 << 20
	// savedDecoderMaxMemory caps what the decompressor may allocate for one.
	savedDecoderMaxMemory = 64 << 20
	// savedMaxRecords caps the records of a saved layer. A record is far
	// larger in memory than the few bytes of JSON it can be written in, so
	// the byte cap alone does not bound memory.
	savedMaxRecords = 1 << 18

	// The record kinds, as a saved layer spells them.
	savedNode     = "node"
	savedHardlink = "hardlink"
	savedWhiteout = "whiteout"
	savedOpaque   = "opaque"
)

// EncodeLayer returns a scanned layer in the form DecodeLayer reads back: a
// zstd frame, checksummed, holding a JSON header — the format version, the
// size of the layer's compressed blob and its diff_id — followed by one JSON
// object per record, in stream order. A record keeps everything the scan
// captured but the fs-verity digest of a stored file: its kind and name, a
// hardlink's target, the node's metadata, a stored file's content digest and
// size, and the bytes of a file small enough to be inlined. Names, link
// targets and extended attributes are encoded as bytes, since none of them
// need be valid UTF-8. A layer whose encoding DecodeLayer would refuse for
// its size or its number of records is not encoded. The layer must be encoded
// before it is merged: a merged tree shares the layer's nodes and changes
// them.
func EncodeLayer(l *Layer, blobSize int64, diffID digest.Digest) ([]byte, error) {
	if len(l.records) > savedMaxRecords {
		return nil, fmt.Errorf("rootfs: encode layer %d: %d records exceed the limit of %d", l.idx, len(l.records), savedMaxRecords)
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, fmt.Errorf("rootfs: encode layer %d: %w", l.idx, err)
	}
	raw := &countWriter{w: zw}
	enc := json.NewEncoder(raw)
	err = enc.Encode(savedHeader{Version: savedVersion, BlobSize: blobSize, DiffID: diffID})
	for _, rec := range l.records {
		if err != nil || raw.n > savedMaxBytes {
			break
		}
		err = enc.Encode(savedFromRecord(rec))
	}
	// The encoder is closed on a failure too, which releases it.
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("rootfs: encode layer %d: %w", l.idx, err)
	}
	if raw.n > savedMaxBytes || buf.Len() > savedMaxBytes {
		return nil, fmt.Errorf("rootfs: encode layer %d: the file list exceeds the limit of %d bytes", l.idx, savedMaxBytes)
	}
	return buf.Bytes(), nil
}

// DecodeLayer returns layer idx as EncodeLayer saved it: the records the scan
// yielded, in the same order, every node placed at idx. A stored file's
// payload is pending — its content digest and size with no fs-verity digest —
// as in a layer made from a table of contents, so the builder the layer is
// merged into resolves the payloads that survive the merge; an inlined file
// carries its bytes. The data is refused unless it was written in the known
// version for a blob of blobSize bytes with this diffID, stays within the
// size and record bounds, every record is one a scan can yield, and the frame
// decompresses to its end, where its checksum is verified.
func DecodeLayer(data []byte, idx int, blobSize int64, diffID digest.Digest) (*Layer, error) {
	if len(data) > savedMaxBytes {
		return nil, fmt.Errorf("rootfs: saved layer %d: %d bytes exceed the limit of %d", idx, len(data), savedMaxBytes)
	}
	zr, err := zstd.NewReader(
		bytes.NewReader(data),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(savedDecoderMaxMemory),
		zstd.WithDecodeBuffersBelow(0),
	)
	if err != nil {
		return nil, fmt.Errorf("rootfs: saved layer %d: %w", idx, err)
	}
	defer zr.Close()
	// One byte past the cap tells a longer stream from one that ends there.
	raw := &countReader{r: io.LimitReader(zr, savedMaxBytes+1)}
	dec := json.NewDecoder(raw)
	dec.DisallowUnknownFields()

	var hdr savedHeader
	if err := dec.Decode(&hdr); err != nil {
		return nil, fmt.Errorf("rootfs: saved layer %d: read header: %w", idx, err)
	}
	switch {
	case hdr.Version != savedVersion:
		return nil, fmt.Errorf("rootfs: saved layer %d: unknown version %d", idx, hdr.Version)
	case hdr.BlobSize != blobSize:
		return nil, fmt.Errorf("rootfs: saved layer %d: saved for a blob of %d bytes, not %d", idx, hdr.BlobSize, blobSize)
	case hdr.DiffID != diffID:
		return nil, fmt.Errorf("rootfs: saved layer %d: saved for diff_id %s, not %s", idx, hdr.DiffID, diffID)
	}

	l := &Layer{idx: idx}
	for {
		var s savedRecord
		err := dec.Decode(&s)
		if raw.n > savedMaxBytes {
			return nil, fmt.Errorf("rootfs: saved layer %d: the file list exceeds the limit of %d bytes", idx, savedMaxBytes)
		}
		// Within the cap, the read that finds the end of the frame is the one
		// that checks its checksum, so only a clean end of stream ends the
		// layer.
		if errors.Is(err, io.EOF) {
			return l, nil
		}
		if err != nil {
			return nil, fmt.Errorf("rootfs: saved layer %d: read record %d: %w", idx, len(l.records), err)
		}
		if len(l.records) == savedMaxRecords {
			return nil, fmt.Errorf("rootfs: saved layer %d: more than %d records", idx, savedMaxRecords)
		}
		rec, err := s.record(idx)
		if err != nil {
			return nil, fmt.Errorf("rootfs: saved layer %d: record %d: %w", idx, len(l.records), err)
		}
		l.records = append(l.records, rec)
	}
}

// Payloads returns the payloads the layer's regular files name by content
// digest, one per file and in stream order: the files kept as objects, and in
// a layer made from a table of contents the pending inline ones too.
func (l *Layer) Payloads() []Payload {
	var payloads []Payload
	for _, rec := range l.records {
		if rec.n != nil && rec.n.digest != "" {
			payloads = append(payloads, Payload{Digest: rec.n.digest, Size: rec.n.size})
		}
	}
	return payloads
}

// The JSON documents of a saved layer.
type (
	savedHeader struct {
		Version  int           `json:"version"`
		BlobSize int64         `json:"blobSize"`
		DiffID   digest.Digest `json:"diffID"`
	}

	// savedRecord is one record. Kind and name are always present; target is
	// a hardlink's, every other field a node's, and a field holding its zero
	// value is left out. A whiteout's target is not written: its name gives
	// it.
	savedRecord struct {
		Kind      string        `json:"kind"`
		Name      []byte        `json:"name"`
		Target    []byte        `json:"target,omitempty"`
		Mode      uint32        `json:"mode,omitempty"`
		UID       int           `json:"uid,omitempty"`
		GID       int           `json:"gid,omitempty"`
		Xattrs    []savedXattr  `json:"xattrs,omitempty"`
		Linkname  []byte        `json:"linkname,omitempty"`
		Content   []byte        `json:"content,omitempty"`
		Digest    digest.Digest `json:"digest,omitempty"`
		Size      int64         `json:"size,omitempty"`
		MtimeSec  int64         `json:"mtime,omitempty"`
		MtimeNsec int64         `json:"mtimeNsec,omitempty"`
		Devmajor  int64         `json:"devmajor,omitempty"`
		Devminor  int64         `json:"devminor,omitempty"`
	}

	savedXattr struct {
		Key   []byte `json:"key"`
		Value []byte `json:"value"`
	}

	// countWriter counts the bytes written through it.
	countWriter struct {
		w io.Writer
		n int64
	}
)

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// savedFromRecord converts a scanned record into its saved form. Extended
// attributes are written in key order, so one layer always encodes to the
// same bytes.
func savedFromRecord(rec record) savedRecord {
	s := savedRecord{Name: []byte(rec.name)}
	switch rec.kind {
	case kindHardlink:
		s.Kind, s.Target = savedHardlink, []byte(rec.target)
		return s
	case kindWhiteout:
		s.Kind = savedWhiteout
		return s
	case kindOpaque:
		s.Kind = savedOpaque
		return s
	}
	n := rec.n
	s.Kind = savedNode
	s.Mode = uint32(n.mode)
	s.UID, s.GID = n.uid, n.gid
	for _, key := range slices.Sorted(maps.Keys(n.xattrs)) {
		s.Xattrs = append(s.Xattrs, savedXattr{Key: []byte(key), Value: []byte(n.xattrs[key])})
	}
	s.Linkname = []byte(n.linkname)
	s.Content, s.Digest, s.Size = n.content, n.digest, n.size
	s.MtimeSec, s.MtimeNsec = n.mtimeSec, n.mtimeNsec
	s.Devmajor, s.Devminor = n.devmajor, n.devminor
	return s
}

// record converts a saved record back into the record of layer idx it was
// made from, accepting only what a scan yields for an entry of that name. A
// marker is told by its base name, as a scan tells it, carries nothing else,
// and derives what it removes from that name; no other kind may bear a
// marker's name. A hardlink carries a target and nothing else, and only a
// node carries a node's fields.
func (s savedRecord) record(idx int) (record, error) {
	name := string(s.Name)
	if !isNormalized(name) {
		return record{}, fmt.Errorf("name %q is not a normalized path", name)
	}
	dir, base := splitPath(name)
	opaque := base == ".wh..wh..opq"
	whiteout := !opaque && strings.HasPrefix(base, ".wh.")
	target := string(s.Target)
	switch s.Kind {
	case savedOpaque:
		if !opaque || target != "" || s.hasNodeFields() {
			return record{}, fmt.Errorf("malformed opaque marker %q", name)
		}
		return record{kind: kindOpaque, name: name}, nil
	case savedWhiteout:
		removed := strings.TrimPrefix(base, ".wh.")
		if !whiteout || removed == "" || target != "" || s.hasNodeFields() {
			return record{}, fmt.Errorf("malformed whiteout %q", name)
		}
		return record{kind: kindWhiteout, name: name, target: path.Join(dir, removed)}, nil
	case savedHardlink:
		// A link to the root has an empty target, as a scan records it.
		if opaque || whiteout || s.hasNodeFields() || (target != "" && !isNormalized(target)) {
			return record{}, fmt.Errorf("malformed hardlink %q to %q", name, target)
		}
		return record{kind: kindHardlink, name: name, target: target}, nil
	case savedNode:
		if opaque || whiteout || target != "" {
			return record{}, fmt.Errorf("node %q bears a marker's name or a target", name)
		}
		n, err := s.node(name, idx)
		if err != nil {
			return record{}, err
		}
		return record{kind: kindNode, name: name, n: n}, nil
	default:
		return record{}, fmt.Errorf("unknown kind %q for %q", s.Kind, name)
	}
}

// hasNodeFields reports whether s sets any field only a node record carries.
func (s savedRecord) hasNodeFields() bool {
	return s.Mode != 0 || s.UID != 0 || s.GID != 0 || len(s.Xattrs) != 0 || len(s.Linkname) != 0 ||
		len(s.Content) != 0 || s.Digest != "" || s.Size != 0 || s.MtimeSec != 0 || s.MtimeNsec != 0 ||
		s.Devmajor != 0 || s.Devminor != 0
}

// node rebuilds the node a saved record installs at name in layer idx: one of
// the types a scan accepts, with the mode bits and the modification time a
// scan gives a node. Only a regular file carries a payload, only a symlink a
// link target, which it must have, and only a device or fifo device numbers.
func (s savedRecord) node(name string, idx int) (*node, error) {
	n := &node{
		mode:      os.FileMode(s.Mode),
		uid:       s.UID,
		gid:       s.GID,
		linkname:  string(s.Linkname),
		size:      s.Size,
		mtimeSec:  s.MtimeSec,
		mtimeNsec: s.MtimeNsec,
		devmajor:  s.Devmajor,
		devminor:  s.Devminor,
		layer:     idx,
	}
	for _, x := range s.Xattrs {
		key := string(x.Key)
		// A PAX record cannot carry such a key, so no scan yields one.
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("%q has extended attribute key %q", name, key)
		}
		if n.xattrs == nil {
			n.xattrs = make(map[string]string, len(s.Xattrs))
		}
		if _, ok := n.xattrs[key]; ok {
			return nil, fmt.Errorf("%q repeats extended attribute %q", name, key)
		}
		n.xattrs[key] = string(x.Value)
	}
	if n.mode&^(os.ModeType|os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, fmt.Errorf("%q has mode bits no scan sets: %s", name, n.mode)
	}
	if s.MtimeSec < 0 || s.MtimeNsec < 0 || s.MtimeNsec >= 1e9 {
		return nil, fmt.Errorf("%q has modification time %d.%09d", name, s.MtimeSec, s.MtimeNsec)
	}
	var regular, symlink, device bool
	switch n.mode.Type() {
	case 0:
		regular = true
	case os.ModeSymlink:
		symlink = true
	case os.ModeDevice, os.ModeDevice | os.ModeCharDevice, os.ModeNamedPipe:
		device = true
	case os.ModeDir:
	default:
		return nil, fmt.Errorf("unsupported file type %s for %q", n.mode.Type(), name)
	}
	if symlink != (len(s.Linkname) != 0) {
		return nil, fmt.Errorf("%q of type %s has link target %q", name, n.mode.Type(), s.Linkname)
	}
	if !device && (s.Devmajor != 0 || s.Devminor != 0) {
		return nil, fmt.Errorf("%q is not a device but carries device numbers", name)
	}
	if !regular {
		if s.Digest != "" || len(s.Content) != 0 || s.Size != 0 {
			return nil, fmt.Errorf("%q is not a regular file but carries a payload", name)
		}
		return n, nil
	}
	if err := s.payload(name, n); err != nil {
		return nil, err
	}
	return n, nil
}

// payload fills in the payload of the regular file n at name: inlined, with
// exactly its size in bytes and no more than an inlined file may have, or
// stored, with a valid content digest, no bytes, and a size over the inline
// threshold.
func (s savedRecord) payload(name string, n *node) error {
	if s.Digest == "" {
		if int64(len(s.Content)) != s.Size || s.Size > composefs.InlineContentMax {
			return fmt.Errorf("regular file %q of %d bytes has %d bytes of content and no content digest", name, s.Size, len(s.Content))
		}
		// An inlined file's content is never nil, even when it is empty.
		n.content = append([]byte{}, s.Content...)
		return nil
	}
	if err := digestutil.ValidateSHA256(s.Digest); err != nil {
		return fmt.Errorf("content digest of %q: %w", name, err)
	}
	if len(s.Content) != 0 || s.Size <= composefs.InlineContentMax {
		return fmt.Errorf("stored file %q of %d bytes is one a scan inlines", name, s.Size)
	}
	n.digest = s.Digest
	return nil
}

// isNormalized reports whether name is a path as a scan normalizes one.
func isNormalized(name string) bool {
	clean, err := normalizeName(name)
	return err == nil && clean == name
}
