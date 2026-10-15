// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"archive/tar"
	"fmt"

	"github.com/foundriesio/ocicfs/internal/zstdchunked"
)

// LayerFromTOC returns layer idx as its zstd:chunked table of contents
// describes it: the records a Scan of the same layer's tar stream yields, in
// the same order, without reading the layer. A regular file's bytes are not
// in the table, so its payload is pending: an empty file is complete, and any
// other carries its content digest and size with neither content nor an
// fs-verity digest. The builder the layer is merged into resolves the payloads
// that survive the merge.
func LayerFromTOC(toc *zstdchunked.TOC, idx int) (*Layer, error) {
	l := &Layer{idx: idx}
	for _, e := range toc.Entries {
		name, err := normalizeName(e.Name)
		if err != nil {
			return nil, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		if name == "" {
			continue
		}
		hdr, err := headerFromEntry(e)
		if err != nil {
			return nil, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		rec, err := recordFromHeader(hdr, name, idx, func(n *node) error {
			if e.Size == 0 {
				n.content = []byte{}
				return nil
			}
			if e.Digest == "" {
				return fmt.Errorf("regular file %s has no content digest", name)
			}
			n.digest = e.Digest
			n.size = e.Size
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		l.records = append(l.records, rec)
	}
	return l, nil
}

// headerFromEntry returns the tar header a table-of-contents entry was made
// from, as far as a record needs it, so an entry converts through the same
// code as the tar entry it describes. Extended attributes go back under their
// PAX record names, which is where that code reads them.
func headerFromEntry(e zstdchunked.Entry) (*tar.Header, error) {
	var typeflag byte
	switch e.Type {
	case zstdchunked.TypeReg:
		typeflag = tar.TypeReg
	case zstdchunked.TypeHardlink:
		typeflag = tar.TypeLink
	case zstdchunked.TypeSymlink:
		typeflag = tar.TypeSymlink
	case zstdchunked.TypeDir:
		typeflag = tar.TypeDir
	case zstdchunked.TypeChar:
		typeflag = tar.TypeChar
	case zstdchunked.TypeBlock:
		typeflag = tar.TypeBlock
	case zstdchunked.TypeFifo:
		typeflag = tar.TypeFifo
	default:
		return nil, fmt.Errorf("unsupported entry type %q for %s", e.Type, e.Name)
	}
	hdr := &tar.Header{
		Typeflag: typeflag,
		Name:     e.Name,
		Linkname: e.Linkname,
		Size:     e.Size,
		Mode:     e.Mode,
		Uid:      e.UID,
		Gid:      e.GID,
		Devmajor: e.Devmajor,
		Devminor: e.Devminor,
	}
	// A header without a modification time keeps the zero time, which the
	// conversion clamps like any other time before the epoch.
	if e.ModTime != nil {
		hdr.ModTime = *e.ModTime
	}
	if len(e.Xattrs) > 0 {
		hdr.PAXRecords = make(map[string]string, len(e.Xattrs))
		for k, v := range e.Xattrs {
			hdr.PAXRecords["SCHILY.xattr."+k] = v
		}
	}
	return hdr, nil
}
