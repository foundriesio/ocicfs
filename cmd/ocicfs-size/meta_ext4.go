// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"sort"
	"strings"
)

// The ext4 host profile: mke2fs defaults with 4 KiB blocks, 256-byte inodes
// with 32 bytes of extra inode fields, metadata_csum, and neither
// inline_data nor ea_inode.
const (
	ext4DirTail        = 12                 // the checksum tail of a directory block
	ext4DirUsable      = 4096 - ext4DirTail // dirent bytes a directory block holds
	ext4InodeXattr     = 256 - 128 - 32 - 4 // in-inode xattr bytes past the header magic
	ext4BlockXattr     = 4096 - 32          // xattr block bytes past its header
	ext4FastSymlinkMax = 59                 // a target this long or shorter lives in the inode
	ext4HtreeFill      = 0.75               // assumed leaf fill of a hashed directory
	ext4XattrEntry     = 16                 // struct ext4_xattr_entry
	ext4XattrEnd       = 4                  // the zero word ending an entry list
	ext4SnapshotDirs   = 3                  // a snapshot's own directory, fs/ and work/
	ext4DotDot         = 12 + 12            // the . and .. entries of a directory
)

// ext4DirBlocks returns the blocks of an ext4 directory holding entries of
// the given name lengths. One block is linear; a larger directory is hashed:
// a root index block plus leaves filled to ext4HtreeFill.
func ext4DirBlocks(nameLens []int) int64 {
	var total int64
	used, blocks := int64(ext4DotDot), int64(1)
	for _, n := range nameLens {
		rec := ext4RecLen(n)
		total += rec
		if used+rec > ext4DirUsable {
			blocks++
			used = 0
		}
		used += rec
	}
	if blocks == 1 {
		return 1
	}
	return 1 + int64(math.Ceil(float64(total)/(ext4DirUsable*ext4HtreeFill)))
}

// ext4RecLen is the size of a directory entry naming n bytes.
func ext4RecLen(n int) int64 {
	return int64(8+n+3) &^ 3
}

// ext4XattrSize returns the bytes an inode's extended attributes take in an
// xattr area, entries and values together, without the area's end marker.
func ext4XattrSize(xattrs []Xattr) int64 {
	var size int64
	for _, x := range xattrs {
		size += int64(ext4XattrEntry+len(ext4XattrSuffix(x.Name))+3)&^3 + int64(len(x.Value)+3)&^3
	}
	return size
}

// ext4XattrPlacement reports where an inode's extended attributes go: in
// the inode (block false), in one xattr block (block true), or nowhere,
// when they fit neither and setting them fails with ENOSPC (fit false).
func ext4XattrPlacement(xattrs []Xattr) (block, fit bool) {
	if len(xattrs) == 0 {
		return false, true
	}
	size := ext4XattrSize(xattrs) + ext4XattrEnd
	switch {
	case size <= ext4InodeXattr:
		return false, true
	case size <= ext4BlockXattr:
		return true, true
	default:
		return false, false
	}
}

// ext4XattrKey identifies an xattr set for block sharing: ext4 keeps one
// block for every inode with the same attributes.
func ext4XattrKey(xattrs []Xattr) string {
	sorted := append([]Xattr(nil), xattrs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	for _, x := range sorted {
		b.WriteString(x.Name)
		b.WriteByte(0)
		b.WriteString(x.Value)
		b.WriteByte(0)
	}
	return b.String()
}

// ext4XattrSuffix returns the part of an xattr name ext4 stores after the
// prefix its name index stands for.
func ext4XattrSuffix(name string) string {
	for _, p := range []string{"system.posix_acl_access", "system.posix_acl_default", "system.richacl", "user.", "trusted.", "security.", "system."} {
		if s, ok := strings.CutPrefix(name, p); ok {
			return s
		}
	}
	return name
}
