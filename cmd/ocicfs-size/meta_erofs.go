// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math/bits"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/composefs"
)

// The EROFS on-disk constants both writers share.
const (
	erofsBlock          = 4096
	erofsSuperEnd       = 1024 + 128 // the superblock, at 1 KiB
	erofsSlot           = 32         // inode slot size
	erofsCompactInode   = 32
	erofsExtendedInode  = 64
	erofsXattrHeader    = 12 // struct erofs_xattr_ibody_header
	erofsXattrEntry     = 4  // struct erofs_xattr_entry
	erofsDirent         = 12 // struct erofs_dirent
	erofsChunkIndex     = 4  // a block-map chunk entry
	erofsMaxChunkBits   = 31 // EROFS_CHUNK_FORMAT_BLKBITS_MASK
	erofsBlockBits      = 12
	composefsMaxShared  = 0x80 // shared xattrs an inode may reference
	overlayMetacopySize = 4 + 32
)

// erofsXattrSuffix returns the part of an xattr name EROFS stores after the
// prefix its name index stands for; known is false for a name no index
// covers.
func erofsXattrSuffix(name string) (suffix string, known bool) {
	for _, p := range []string{"user.", "system.posix_acl_access", "system.posix_acl_default", "trusted.", "security."} {
		if s, ok := strings.CutPrefix(name, p); ok {
			return s, true
		}
	}
	return name, false
}

// erofsXattrEntrySize is the 4-byte aligned size of one xattr entry.
func erofsXattrEntrySize(x Xattr) int64 {
	s, _ := erofsXattrSuffix(x.Name)
	return roundUp(int64(erofsXattrEntry+len(s)+len(x.Value)), 4)
}

// chunkCount returns the number of chunks a chunk-based EROFS file of size
// bytes is split into: chunks are the next power of two at or above the
// size, at least a block and at most 2^31 blocks.
func chunkCount(size int64) int64 {
	cb := int64(bits.Len64(uint64(size - 1))) // #nosec G115 -- callers pass a positive size
	if cb < erofsBlockBits {
		cb = erofsBlockBits
	}
	if cb-erofsBlockBits > erofsMaxChunkBits {
		cb = erofsMaxChunkBits + erofsBlockBits
	}
	return (size + 1<<cb - 1) >> cb
}

func roundUp(v, a int64) int64 { return (v + a - 1) / a * a }

// ---- composefs-c 1.0.8 (mkcomposefs --min-version=1 --max-version=1) ----

// cfsNode is one inode of a composefs image as the writer lays it out.
type cfsNode struct {
	name      string
	mode      os.FileMode
	uid, gid  int
	nlink     int
	size      int64
	mtime     [2]int64
	inline    bool // regular file whose bytes are in the image
	linkname  string
	verity    string // hex fs-verity digest of an external file
	rdev0     bool   // character device 0:0, an overlay whiteout
	xattrs    []Xattr
	children  []*cfsNode // sorted by name; hardlinks included
	linkTo    *cfsNode   // set on a hardlink name
	parent    *cfsNode
	nBlocks   int64
	tail      int64
	compact   bool
	xattrSize int64
}

// composefsInlineCap is the most file content mkcomposefs 1.0.8 accepts
// inline (LCFS_INLINE_CONTENT_MAX); a larger threshold is sized with the
// same EROFS flat-inline layout.
const composefsInlineCap = 5000

// composefsImageSize returns the size of the composefs image mkcomposefs
// writes for entries, by laying out its inodes, shared xattrs and data
// blocks without writing them. Regular files of at most inlineMax bytes
// are stored in the image, larger ones by digest, whichever way the
// entries carry them.
func composefsImageSize(entries []composefs.Entry, inlineMax int64) (int64, error) {
	root, err := cfsTree(entries, inlineMax)
	if err != nil {
		return 0, err
	}
	cfsRewrite(root, root)
	addRootWhiteouts(root)
	order := cfsBreadthFirst(root)

	min := root.mtime
	for _, n := range order {
		if n.mode.IsDir() {
			n.nlink = 2
			for _, c := range n.children {
				if c.linkTo == nil && c.mode.IsDir() {
					n.nlink++
				}
			}
		}
		if n.mtime[0] < min[0] || (n.mtime[0] == min[0] && n.mtime[1] < min[1]) {
			min = n.mtime
		}
	}
	shared, sharedSize := cfsSharedXattrs(order)

	pos := int64(erofsSuperEnd)
	var dataBlocks int64
	for _, n := range order {
		cfsInodeSize(n)
		n.compact = cfsFitsCompact(n, min)
		isize := int64(erofsExtendedInode)
		if n.compact {
			isize = erofsCompactInode
		}
		var nShared int
		var unshared int64
		for _, x := range n.xattrs {
			if shared[x.Name+"\x00"+x.Value] && nShared < composefsMaxShared {
				nShared++
			} else {
				unshared += erofsXattrEntrySize(x)
			}
		}
		if nShared > 0 || unshared > 0 {
			n.xattrSize = roundUp(erofsXattrHeader+int64(nShared)*erofsXattrEntry+unshared, 4)
		}
		pos = roundUp(pos, erofsSlot)
		pos += cfsTailPadding(n, pos, isize+n.xattrSize)
		dataBlocks += n.nBlocks
		pos += isize + n.xattrSize + n.tail
	}
	inodesEnd := roundUp(pos, erofsSlot)
	return roundUp(inodesEnd+sharedSize, erofsBlock) + dataBlocks*erofsBlock, nil
}

// cfsTree builds the composefs node tree mkcomposefs parses from the dump of
// entries, adding the root the dump adds when entries lack one, with
// regular files of at most inlineMax bytes inline.
func cfsTree(entries []composefs.Entry, inlineMax int64) (*cfsNode, error) {
	byPath := map[string]*cfsNode{}
	sorted := append([]composefs.Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	if len(sorted) == 0 || sorted[0].Path != "/" {
		sorted = append([]composefs.Entry{{Path: "/", Mode: os.ModeDir | 0o755}}, sorted...)
	}
	for _, e := range sorted {
		n := &cfsNode{name: path.Base(e.Path)}
		if e.Hardlink != "" {
			t, ok := byPath[e.Hardlink]
			if !ok {
				return nil, fmt.Errorf("composefs: hardlink %s to absent %s", e.Path, e.Hardlink)
			}
			n.linkTo = t
		} else {
			n.mode, n.uid, n.gid, n.nlink = e.Mode, e.UID, e.GID, max(e.Nlink, 1)
			n.size, n.mtime, n.linkname = e.Size, [2]int64{e.Mtime, e.MtimeNsec}, e.Linkname
			if e.Content != nil {
				n.size = int64(len(e.Content))
			}
			n.inline = e.Mode.IsRegular() && n.size <= inlineMax
			switch {
			case n.inline:
			case e.Mode&os.ModeSymlink != 0:
				n.size = int64(len(e.Linkname))
			case !e.Mode.IsRegular():
				n.size = 0
			}
			// Only the digest's length, and which files share one, matter
			// for sizing; the content digest stands in when there is no
			// fs-verity digest.
			switch {
			case e.Verity != "":
				n.verity = e.Verity.Encoded()
			case e.Digest != "":
				n.verity = e.Digest.Encoded()
			case e.Mode.IsRegular() && !n.inline:
				n.verity = digest.FromBytes(e.Content).Encoded()
			}
			n.rdev0 = e.Mode&os.ModeCharDevice != 0 && e.Devmajor == 0 && e.Devminor == 0
			for k, v := range e.Xattrs {
				n.xattrs = append(n.xattrs, Xattr{Name: k, Value: v})
			}
		}
		byPath[e.Path] = n
		if e.Path == "/" {
			continue
		}
		parent, ok := byPath[path.Dir(e.Path)]
		if !ok || !parent.mode.IsDir() {
			return nil, fmt.Errorf("composefs: %s has no parent directory", e.Path)
		}
		n.parent = parent
		parent.children = append(parent.children, n)
	}
	return byPath["/"], nil
}

// cfsRewrite applies the writer's overlay rewrite to the tree under n:
// escaping trusted.overlay xattrs, adding metacopy and redirect xattrs to
// external files, turning 0:0 character devices into escaped whiteouts,
// and adding . and .. to every directory.
func cfsRewrite(n, parent *cfsNode) {
	for i, x := range n.xattrs {
		if rest, ok := strings.CutPrefix(x.Name, "trusted.overlay."); ok {
			n.xattrs[i].Name = "trusted.overlay.overlay." + rest
		}
	}
	if n.mode.IsRegular() && n.size > 0 && !n.inline {
		// The values only need the real lengths and to be equal exactly when
		// the digests are.
		v := n.verity + strings.Repeat("0", 64-min(len(n.verity), 64))
		n.setXattr("trusted.overlay.metacopy", "\x00\x24\x00\x01"+v[:overlayMetacopySize-4])
		n.setXattr("trusted.overlay.redirect", "/"+v[:2]+"/"+v[2:])
	}
	if n.rdev0 && n.parent != nil {
		n.mode, n.size = n.mode.Perm(), 0
		n.setXattr("trusted.overlay.overlay.whiteout", "")
		n.setXattr("user.overlay.whiteout", "")
		n.parent.setXattr("trusted.overlay.overlay.whiteouts", "")
		n.parent.setXattr("user.overlay.whiteouts", "")
		n.parent.setXattr("trusted.overlay.overlay.opaque", "x")
		n.parent.setXattr("user.overlay.opaque", "x")
	}
	if !n.mode.IsDir() {
		return
	}
	children := slices.Clone(n.children) // sorting n.children below must not reorder these
	n.children = append(n.children, &cfsNode{name: ".", linkTo: n}, &cfsNode{name: "..", linkTo: parent})
	sort.Slice(n.children, func(i, j int) bool { return n.children[i].name < n.children[j].name })
	for _, c := range children {
		if c.linkTo == nil {
			cfsRewrite(c, n)
		}
	}
}

// addRootWhiteouts marks the root opaque and gives it the 256 whiteout
// devices 00 to ff that a version 1 image carries, unless a name is taken.
func addRootWhiteouts(root *cfsNode) {
	root.setXattr("trusted.overlay.opaque", "y")
	taken := map[string]bool{}
	for _, c := range root.children {
		taken[c.name] = true
	}
	for i := 0; i < 256; i++ {
		name := fmt.Sprintf("%02x", i)
		if taken[name] {
			continue
		}
		root.children = append(root.children, &cfsNode{
			name: name, mode: os.ModeDevice | os.ModeCharDevice | 0o644, uid: root.uid, gid: root.gid,
			nlink: 1, mtime: root.mtime, parent: root,
		})
	}
	sort.Slice(root.children, func(i, j int) bool { return root.children[i].name < root.children[j].name })
}

// setXattr sets an xattr, replacing one of the same name.
func (n *cfsNode) setXattr(name, value string) {
	for i := range n.xattrs {
		if n.xattrs[i].Name == name {
			n.xattrs[i].Value = value
			return
		}
	}
	n.xattrs = append(n.xattrs, Xattr{Name: name, Value: value})
}

// cfsBreadthFirst returns the inodes in the order the writer stores them:
// breadth first, children by name, hardlink names skipped.
func cfsBreadthFirst(root *cfsNode) []*cfsNode {
	order := []*cfsNode{root}
	for i := 0; i < len(order); i++ {
		for _, c := range order[i].children {
			if c.linkTo == nil {
				order = append(order, c)
			}
		}
	}
	return order
}

// cfsSharedXattrs returns the xattrs more than one inode carries, which go
// to the shared table, and the table's size.
func cfsSharedXattrs(order []*cfsNode) (map[string]bool, int64) {
	count := map[string]int{}
	entry := map[string]int64{}
	for _, n := range order {
		for _, x := range n.xattrs {
			k := x.Name + "\x00" + x.Value
			count[k]++
			entry[k] = erofsXattrEntrySize(x)
		}
	}
	shared := map[string]bool{}
	var size int64
	for k, c := range count {
		if c > 1 {
			shared[k] = true
			size += entry[k]
		}
	}
	return shared, size
}

// cfsInodeSize sets a node's data blocks and inline tail.
func cfsInodeSize(n *cfsNode) {
	n.nBlocks, n.tail = 0, 0
	switch {
	case n.mode.IsDir():
		var used int64
		for _, c := range n.children {
			l := int64(erofsDirent + len(c.name))
			if used+l > erofsBlock {
				n.nBlocks++
				used = 0
			}
			used += l
		}
		if used > erofsBlock/2 {
			n.nBlocks++
			used = 0
		}
		n.tail = used
	case n.mode&os.ModeSymlink != 0:
		n.tail = int64(len(n.linkname))
	case n.mode.IsRegular() && n.size > 0 && n.inline:
		n.nBlocks, n.tail = n.size/erofsBlock, n.size%erofsBlock
		if n.tail > erofsBlock/2 {
			n.nBlocks++
			n.tail = 0
		}
	case n.mode.IsRegular() && n.size > 0:
		n.tail = chunkCount(n.size) * erofsChunkIndex
	}
}

// cfsFitsCompact reports whether a node fits a compact inode.
func cfsFitsCompact(n *cfsNode, min [2]int64) bool {
	size := n.size
	if n.mode.IsDir() {
		size = n.nBlocks*erofsBlock + n.tail
	}
	return n.mtime == min && n.nlink <= 0xffff && n.uid <= 0xffff && n.gid <= 0xffff && n.uid >= 0 && n.gid >= 0 && size <= 0xffffffff
}

// cfsTailPadding returns the padding before an inode at pos that keeps its
// inline tail within one block, moving the tail to a data block when it
// cannot.
func cfsTailPadding(n *cfsNode, pos, head int64) int64 {
	total := head + n.tail
	if n.mode&os.ModeSymlink != 0 {
		if total/erofsBlock > 0 {
			n.nBlocks++
			n.tail = 0
		}
		if pos/erofsBlock != (pos+total-1)/erofsBlock {
			return roundUp(pos, erofsBlock) - pos
		}
		return 0
	}
	rem := erofsBlock - (pos+head)%erofsBlock
	if rem >= n.tail {
		return 0
	}
	pad := roundUp(rem, erofsSlot)
	if n.tail <= erofsBlock-(pos+head+pad)%erofsBlock {
		return pad
	}
	n.nBlocks++
	n.tail = 0
	return roundUp(pos, erofsBlock) - pos
}

// ---- mkfs.erofs 1.9.4 (--tar=f --aufs -Enoinline_data -b4096, or
// --tar=headerball --aufs -b4096) ----

// mkfsInode is one inode of the tree mkfs.erofs builds from a tar.
type mkfsInode struct {
	kind      Kind // KindWhiteout for an overlay whiteout device
	size      int64
	nlink     int
	uid, gid  uint32
	epochTime bool // its mtime is the build time, which keeps it compact
	xattrs    []Xattr
	whiteouts bool          // a directory holding whiteouts
	dentries  []*mkfsDentry // a directory's entries, . and .. excluded
	parent    *mkfsInode
	visited   bool
	ibh       *erofsBH // the inode's buffer
	inline    *erofsBH // its inline tail, when it has one
	lazyTail  bool
}

type mkfsDentry struct {
	name  string
	inode *mkfsInode
}

// mkfsErofsImageSize returns the size of the EROFS image mkfs.erofs builds
// from a layer tar with the containerd EROFS differ's flags, or, with
// headerball, the metadata-only image of the same tar whose regular files
// keep their sizes but no data. It fails for a tar mkfs.erofs refuses.
func mkfsErofsImageSize(inv *LayerInv, headerball bool) (int64, error) {
	root, err := mkfsTree(inv)
	if err != nil {
		return 0, err
	}
	m := newErofsBufMgr()
	sb := m.balloc(erofsMeta, 0, 0, 1)
	m.balloon(sb, erofsSuperEnd)
	m.mapbh(sb.bb)

	w := &mkfsWriter{m: m, headerball: headerball}
	w.handle(root, root)
	m.mapbh(root.ibh.bb)
	root.visited = true
	queue := []*mkfsInode{root}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		var subdirs []*mkfsInode
		for _, d := range dir.dentries {
			if d.inode.visited {
				continue
			}
			d.inode.visited = true
			w.handle(d.inode, dir)
			if d.inode.kind == KindDir {
				subdirs = append(subdirs, d.inode)
			}
		}
		queue = append(subdirs, queue...)
		w.writeDir(dir)
	}
	return m.mapbh(nil) * erofsBlock, nil
}

// mkfsTree replays a layer tar the way mkfs.erofs --tar --aufs imports it:
// later names replace earlier ones unless both are directories, missing
// parents are made with their parent's owner and time, .wh. names become
// whiteout devices and opaque markers an xattr.
func mkfsTree(inv *LayerInv) (*mkfsInode, error) {
	root := &mkfsInode{kind: KindDir, nlink: 2, epochTime: true}
	root.parent = root
	byIndex := make(map[int32]*mkfsInode)
	for i, e := range inv.Entries {
		dir, err := mkfsParent(root, e.Path)
		if err != nil {
			return nil, err
		}
		if e.Kind == KindOpaque {
			if e.Path == "/" {
				return nil, fmt.Errorf("mkfs.erofs: an opaque marker at the root is refused")
			}
			dir = mkfsLookupDir(root, e.Path, dir)
			dir.setXattr("trusted.overlay.opaque", "y")
			continue
		}
		if e.Path == "/" {
			if e.Kind != KindDir {
				return nil, fmt.Errorf("mkfs.erofs: root is not a directory")
			}
			root.fill(e)
			continue
		}
		name := path.Base(e.Path)
		d := dir.dentry(name)
		if e.Kind == KindHardlink {
			target, ok := byIndex[e.Link]
			if !ok || e.Link < 0 {
				return nil, fmt.Errorf("mkfs.erofs: hardlink %s to a file outside the layer", e.Path)
			}
			if d != nil && d.inode == target {
				continue
			}
			target.nlink++
			if d == nil {
				dir.dentries = append(dir.dentries, &mkfsDentry{name: name, inode: target})
			} else {
				d.inode.drop()
				d.inode = target
			}
			continue
		}
		if d != nil && d.inode.kind == KindDir && e.Kind == KindDir {
			d.inode.fill(e)
			continue
		}
		in := &mkfsInode{kind: e.Kind, parent: dir}
		in.fill(e)
		if e.Kind == KindWhiteout {
			dir.whiteouts = true
		}
		if e.Kind == KindDir {
			in.nlink = 2
		} else {
			in.nlink = 1
		}
		if d == nil {
			dir.dentries = append(dir.dentries, &mkfsDentry{name: name, inode: in})
		} else {
			d.inode.drop()
			d.inode = in
		}
		if linkable(e.Kind) {
			byIndex[int32(i)] = in
		}
	}
	return root, nil
}

// mkfsParent returns the directory p's parent is, making missing ones.
func mkfsParent(root *mkfsInode, p string) (*mkfsInode, error) {
	dir := root
	parts := strings.Split(strings.Trim(path.Dir(p), "/"), "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		d := dir.dentry(part)
		if d == nil {
			sub := &mkfsInode{kind: KindDir, nlink: 2, parent: dir, uid: dir.uid, gid: dir.gid, epochTime: dir.epochTime}
			dir.dentries = append(dir.dentries, &mkfsDentry{name: part, inode: sub})
			dir = sub
			continue
		}
		if d.inode.kind != KindDir {
			return nil, fmt.Errorf("mkfs.erofs: %s lies beneath a non-directory", p)
		}
		dir = d.inode
	}
	return dir, nil
}

// mkfsLookupDir returns the directory an opaque marker at p names, making it
// when it is missing.
func mkfsLookupDir(root *mkfsInode, p string, parent *mkfsInode) *mkfsInode {
	if d := parent.dentry(path.Base(p)); d != nil && d.inode.kind == KindDir {
		return d.inode
	}
	sub, _ := mkfsParent(root, p+"/x")
	return sub
}

func (in *mkfsInode) dentry(name string) *mkfsDentry {
	for _, d := range in.dentries {
		if d.name == name {
			return d
		}
	}
	return nil
}

// fill takes an inode's metadata from a tar entry.
func (in *mkfsInode) fill(e Entry) {
	in.uid, in.gid, in.epochTime = e.UID, e.GID, false
	switch e.Kind {
	case KindReg:
		in.size = e.Size
	case KindSymlink:
		in.size = int64(len(e.LinkName))
	}
	for _, x := range e.Xattrs {
		if _, ok := erofsXattrSuffix(x.Name); ok {
			in.setXattr(x.Name, x.Value)
		}
	}
}

// drop removes one name of an inode; a directory loses its whole subtree.
func (in *mkfsInode) drop() {
	in.nlink--
	if in.kind == KindDir {
		in.dentries = nil
	}
}

func (in *mkfsInode) setXattr(name, value string) {
	for i := range in.xattrs {
		if in.xattrs[i].Name == name {
			in.xattrs[i].Value = value
			return
		}
	}
	in.xattrs = append(in.xattrs, Xattr{Name: name, Value: value})
}

// mkfsWriter allocates inodes and data the way mkfs.erofs does while it
// walks the tree.
type mkfsWriter struct {
	m          *erofsBufMgr
	headerball bool
}

// handle allocates an inode: a directory's inode with its inline tail, or a
// non-directory's data blocks and inode.
func (w *mkfsWriter) handle(in, parent *mkfsInode) {
	isize := int64(erofsExtendedInode)
	if in.epochTime && in.size <= 0xffffffff && in.uid <= 0xffff && in.gid <= 0xffff && in.nlink <= 0xffff {
		isize = erofsCompactInode
	}
	if in.kind == KindDir {
		in.prepareDir(parent)
		if in.whiteouts {
			in.setXattr("trusted.overlay.origin", "")
		}
	}
	var xattrs int64
	for _, x := range in.xattrs {
		xattrs += erofsXattrEntrySize(x)
	}
	if xattrs > 0 {
		xattrs += erofsXattrHeader
	}
	inodeSize := isize + xattrs
	switch {
	case in.kind == KindDir:
		w.inodeWithTail(in, inodeSize, in.size%erofsBlock)
	case in.kind == KindSymlink:
		w.inodeWithTail(in, inodeSize, in.size%erofsBlock)
		w.tailEnd(in, nil, in.size%erofsBlock, erofsData)
	case in.kind == KindReg && in.size > 0 && w.headerball:
		in.ibh = w.m.balloc(erofsMeta, roundUp(inodeSize, 8)+chunkCount(in.size)*erofsChunkIndex, 0, erofsSlot)
	case in.kind == KindReg && in.size > 0:
		tail := in.size % erofsBlock
		var data *erofsBH
		if full := in.size / erofsBlock; full > 0 {
			data = w.m.balloc(erofsData, full*erofsBlock, 0, erofsBlock)
			w.m.mapbh(data.bb)
			if tail > 0 {
				w.m.balloon(data, erofsBlock)
			}
		}
		in.ibh = w.m.balloc(erofsMeta, inodeSize, 0, erofsSlot)
		if data == nil && tail > 0 {
			data = w.m.balloc(erofsData, erofsBlock, 0, erofsBlock)
			w.m.mapbh(data.bb)
		}
		if data != nil {
			w.m.bdrop(data)
		}
	default:
		in.ibh = w.m.balloc(erofsMeta, inodeSize, 0, erofsSlot)
	}
}

// inodeWithTail allocates an inode with its tail inline, or without it when
// the two do not fit one block, in which case the tail gets a block later.
func (w *mkfsWriter) inodeWithTail(in *mkfsInode, inodeSize, tail int64) {
	if tail == 0 {
		in.ibh = w.m.balloc(erofsMeta, inodeSize, 0, erofsSlot)
		return
	}
	if bh := w.m.balloc(erofsMeta, inodeSize, tail, erofsSlot); bh != nil {
		in.ibh = bh
		in.inline = w.m.battach(bh, tail)
		return
	}
	in.lazyTail = true
	in.ibh = w.m.balloc(erofsMeta, inodeSize, 0, erofsSlot)
}

// tailEnd gives a tail that is not inline its own block.
func (w *mkfsWriter) tailEnd(in *mkfsInode, data *erofsBH, tail int64, typ int) {
	if tail == 0 || in.inline != nil {
		if data != nil {
			w.m.bdrop(data)
		}
		return
	}
	if data == nil {
		data = w.m.balloc(typ, erofsBlock, 0, erofsBlock)
		w.m.mapbh(data.bb)
	} else if in.lazyTail {
		w.m.balloon(data, erofsBlock)
	}
	w.m.bdrop(data)
}

// writeDir writes a directory's blocks after its children's inodes: the
// full blocks, then the lookup of every child's inode, which places it,
// then the tail when it is not inline.
func (w *mkfsWriter) writeDir(dir *mkfsInode) {
	var data *erofsBH
	if full := dir.size / erofsBlock; full > 0 {
		data = w.m.balloc(erofsMeta, full*erofsBlock, 0, erofsBlock)
		w.m.mapbh(data.bb)
	}
	w.m.mapbh(dir.ibh.bb)
	w.m.mapbh(dir.parent.ibh.bb)
	for _, d := range dir.dentries {
		w.m.mapbh(d.inode.ibh.bb)
	}
	w.tailEnd(dir, data, dir.size%erofsBlock, erofsMeta)
}

// prepareDir sorts a directory's entries, with . and .., and sets its size
// and link count.
func (in *mkfsInode) prepareDir(parent *mkfsInode) {
	in.nlink = 2
	names := []string{".", ".."}
	for _, d := range in.dentries {
		names = append(names, d.name)
		if d.inode.kind == KindDir {
			in.nlink++
		}
	}
	sort.Strings(names)
	sort.Slice(in.dentries, func(i, j int) bool { return in.dentries[i].name < in.dentries[j].name })
	var size int64
	for _, n := range names {
		l := int64(erofsDirent + len(n))
		if size%erofsBlock+l > erofsBlock {
			size = roundUp(size, erofsBlock)
		}
		size += l
	}
	in.size = size
	in.parent = parent
}

// ---- the mkfs.erofs buffer manager ----

// The buffer block types the manager keeps apart.
const (
	erofsData = 0
	erofsMeta = 1
)

// erofsBB is a run of blocks being filled; blkaddr is -1 until it is placed.
type erofsBB struct {
	typ        int
	blkaddr    int64
	off        int64
	nblocks    int64
	heads      int
	prev, next *erofsBB
	bucket     int // the fill bucket it is listed in, or -1
}

// erofsBH is one buffer attached to a buffer block.
type erofsBH struct{ bb *erofsBB }

// erofsBufMgr places buffers into blocks the way mkfs.erofs's buffer
// manager does: best fit among partly filled blocks of the same type, placed
// in list order, with data blocks placed at once.
type erofsBufMgr struct {
	head       erofsBB // list sentinel, never placed
	lastMapped *erofsBB
	tail       int64
	buckets    [2][2][erofsBlock][]*erofsBB // [type][placed][fill]
	bitmap     [2][2][erofsBlock / 64]uint64
}

func newErofsBufMgr() *erofsBufMgr {
	m := &erofsBufMgr{}
	m.head.blkaddr = -1
	m.head.prev, m.head.next = &m.head, &m.head
	m.lastMapped = &m.head
	return m
}

func (m *erofsBufMgr) placed(bb *erofsBB) int {
	if bb.blkaddr >= 0 {
		return 1
	}
	return 0
}

// updateBucket moves bb to the tail of the bucket of its current fill, or
// out of every bucket when free.
func (m *erofsBufMgr) updateBucket(bb *erofsBB, free bool) {
	if bb.bucket >= 0 {
		t, p := bb.typ, bb.bucket/erofsBlock
		nr := bb.bucket % erofsBlock
		list := m.buckets[t][p][nr]
		for i, x := range list {
			if x == bb {
				list = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		m.buckets[t][p][nr] = list
		if len(list) == 0 {
			m.setBit(t, p, nr, false)
		}
		bb.bucket = -1
	}
	if free {
		return
	}
	p, nr := m.placed(bb), int(bb.off%erofsBlock)
	m.buckets[bb.typ][p][nr] = append(m.buckets[bb.typ][p][nr], bb)
	m.setBit(bb.typ, p, nr, true)
	bb.bucket = p*erofsBlock + nr
}

// The bitmaps hold bit erofsBlock-1-fill for every non-empty bucket.
func (m *erofsBufMgr) setBit(t, p, nr int, on bool) {
	b := erofsBlock - 1 - nr
	if on {
		m.bitmap[t][p][b/64] |= 1 << (b % 64)
	} else {
		m.bitmap[t][p][b/64] &^= 1 << (b % 64)
	}
}

// nextBit returns the first set bit in [from, limit), or limit.
func (m *erofsBufMgr) nextBit(t, p, limit, from int) int {
	for b := from; b < limit; {
		w := m.bitmap[t][p][b/64] >> (b % 64)
		if w != 0 {
			if r := b + bits.TrailingZeros64(w); r < limit {
				return r
			}
			return limit
		}
		b = (b/64 + 1) * 64
	}
	return limit
}

// attach places size bytes, aligned, at the end of bb, followed by inline
// bytes that must stay in the same block. It returns the fill of bb's last
// block after size, or -1 when the bytes cannot go there.
func (m *erofsBufMgr) attach(bb *erofsBB, size, align, inline int64, dry bool) int64 {
	const mask = erofsBlock - 1
	aligned := roundUp(bb.off, align)
	if (aligned+size+mask)&mask+inline > mask {
		return -1
	}
	tailUpdate := false
	if end := aligned + size + inline; end >= bb.nblocks*erofsBlock {
		oob := end > bb.nblocks*erofsBlock
		if oob && bb.next.blkaddr >= 0 {
			return -1
		}
		if bb.blkaddr >= 0 {
			tailUpdate = m.tail == bb.blkaddr+bb.nblocks
			if oob && !tailUpdate {
				return -1
			}
		}
	}
	if !dry {
		bb.heads++
		bb.off = aligned + size
		bb.nblocks = max(bb.nblocks, (bb.off+mask)/erofsBlock)
		if tailUpdate {
			m.tail = bb.blkaddr + bb.nblocks
		}
		m.updateBucket(bb, false)
	}
	return (aligned+size+mask)&mask + 1
}

// findBucket steps index down to the next non-empty bucket above end.
func (m *erofsBufMgr) findBucket(t, p int, index *int, end int) bool {
	const mask = erofsBlock - 1
	l := *index
	if l <= end {
		return false
	}
	l = mask - (l & mask)
	r := mask - (end & mask)
	if l >= r {
		l = m.nextBit(t, p, erofsBlock, l)
		if l < erofsBlock {
			*index = *index/erofsBlock*erofsBlock + mask - l
			return true
		}
		l = 0
		*index -= erofsBlock
	}
	l = m.nextBit(t, p, r, l)
	if l >= r {
		return false
	}
	*index = *index/erofsBlock*erofsBlock + mask - l
	return true
}

// findForAttach returns the partly filled block the bytes fill best, or nil.
func (m *erofsBufMgr) findForAttach(t int, size, inline, align int64) *erofsBB {
	const mask = erofsBlock - 1
	if align == erofsBlock {
		return nil
	}
	var best *erofsBB
	var usedMax int64
	placed := 2
	used0 := int((erofsBlock - (size+inline)&mask) / align * align)
	end := erofsBlock
	if size+inline >= erofsBlock {
		placed--
	}
	index := used0 + erofsBlock
	for placed > 0 {
		placed--
		for ; m.findBucket(t, placed, &index, end); index-- {
			list := m.buckets[t][placed][index&mask]
			cur := list[0]
			if placed == 1 && cur.next.blkaddr < 0 {
				if len(list) < 2 {
					continue
				}
				cur = list[1]
			}
			ret := m.attach(cur, size, align, inline, true)
			if ret < 0 {
				continue
			}
			if used := ret + inline; used > usedMax {
				usedMax, best = used, cur
			}
			break
		}
		end = used0 + int(align) - 1
		index = used0 + erofsBlock
		if cur := m.lastMapped; placed == 1 && cur != &m.head && cur.typ == t {
			if ret := m.attach(cur, size, align, inline, true); ret >= 0 && ret+inline > usedMax {
				usedMax, best = ret+inline, cur
			}
		}
	}
	return best
}

// balloc attaches size bytes of type t, followed by inline bytes kept in the
// same block, to the best partly filled block or a new one. It returns nil
// when the bytes do not fit one block.
func (m *erofsBufMgr) balloc(t int, size, inline, align int64) *erofsBH {
	bb := m.findForAttach(t, size, inline, align)
	if bb == nil {
		bb = &erofsBB{typ: t, blkaddr: -1, bucket: -1}
		at := m.head.prev
		if t == erofsData {
			at = m.lastMapped
		}
		bb.prev, bb.next = at, at.next
		at.next.prev = bb
		at.next = bb
	}
	if m.attach(bb, size, align, inline, false) < 0 {
		return nil
	}
	return &erofsBH{bb: bb}
}

// battach attaches inline bytes right after bh.
func (m *erofsBufMgr) battach(bh *erofsBH, size int64) *erofsBH {
	m.attach(bh.bb, size, 1, 0, false)
	return &erofsBH{bb: bh.bb}
}

// balloon grows bh's block by incr bytes.
func (m *erofsBufMgr) balloon(bh *erofsBH, incr int64) {
	bh.bb.heads--
	m.attach(bh.bb, incr, 1, 0, false)
}

// mapbh places every block up to and including bb, or all of them for nil,
// and returns the first unused block address.
func (m *erofsBufMgr) mapbh(bb *erofsBB) int64 {
	if bb != nil && bb.blkaddr >= 0 {
		return m.tail
	}
	for t := m.lastMapped.next; t != &m.head; t = t.next {
		if t.blkaddr < 0 {
			t.blkaddr = m.tail
			m.lastMapped = t
			m.updateBucket(t, false)
		}
		m.tail = max(m.tail, t.blkaddr+t.nblocks)
		if t == bb {
			break
		}
	}
	return m.tail
}

// bdrop releases bh and frees its block once no buffer is left in it.
func (m *erofsBufMgr) bdrop(bh *erofsBH) {
	bb := bh.bb
	bb.heads--
	if bb.heads > 0 {
		return
	}
	if bb == m.lastMapped {
		m.lastMapped = bb.prev
	}
	m.updateBucket(bb, true)
	bb.prev.next = bb.next
	bb.next.prev = bb.prev
}
