// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path"
	"strconv"
)

// Profile is the fixed set of storage parameters the models size against.
type Profile struct {
	HostBlock      int64 `json:"hostBlock"`
	ErofsBlock     int64 `json:"erofsBlock"`
	VerityBlock    int64 `json:"verityBlock"`
	InlineMax      int64 `json:"inlineMax"`      // File CAS/image inline threshold
	InlineMaxLayer int64 `json:"inlineMaxLayer"` // File CAS/layer inline threshold
	Verity         bool  `json:"-"`              // fs-verity on every file the model can seal; the report carries both
	HostMetadata   bool  `json:"hostMetadata"`
}

// Breakdown is one model's estimate. Total is exactly PayloadAlloc + Layout
// + Verity + OCIMeta, plus Host when host metadata is included; the other
// numbers explain those.
type Breakdown struct {
	PayloadAlloc int64       `json:"payloadAlloc"`
	Layout       int64       `json:"layout"`
	Verity       int64       `json:"verity"`
	OCIMeta      int64       `json:"ociMeta"`
	Host         int64       `json:"host"`
	Total        int64       `json:"total"`
	PayloadBytes int64       `json:"payloadBytes"`
	Padding      int64       `json:"padding"`
	Inline       int64       `json:"inline"`
	ImagePad     int64       `json:"imagePad"`
	Units        int         `json:"units"`
	Objects      int         `json:"objects,omitempty"` // absent when the model does not count them
	Inodes       int64       `json:"inodes,omitempty"`  // absent when the model does not count them
	Files        int64       `json:"files"`             // regular files the store keeps on the host
	Unused       UnusedStats `json:"unused"`
	Reuse        *Reuse      `json:"reuse,omitempty"`
}

// UnusedStats counts stored file versions no retained root shows.
type UnusedStats struct {
	Versions   int   `json:"versions"`
	Bytes      int64 `json:"bytes"`
	Alloc      int64 `json:"alloc"`
	Whiteouted int   `json:"whiteouted"`
	Superseded int   `json:"superseded"`
}

// total sums the additive components.
func (b *Breakdown) total(p Profile) {
	b.Total = b.PayloadAlloc + b.Layout + b.Verity + b.OCIMeta
	if p.HostMetadata {
		b.Total += b.Host
	}
	b.Padding = b.PayloadAlloc - b.PayloadBytes
}

// snapshot is one stored layer snapshot, the first root and position it
// was met at.
type snapshot struct {
	chainID string
	r       *root
	li      int
}

// snapshots returns the retained set's distinct ChainIDs, bottom-up per
// root in root order.
func snapshots(roots []*root) []snapshot {
	seen := map[string]bool{}
	var out []snapshot
	for _, r := range roots {
		for li, l := range r.img.Layers {
			if !seen[l.ChainID] {
				seen[l.ChainID] = true
				out = append(out, snapshot{chainID: l.ChainID, r: r, li: li})
			}
		}
	}
	return out
}

// scanUnsupported returns why the layer-scan models cannot size the set, or
// "" when they can.
func scanUnsupported(roots []*root) string {
	for _, r := range roots {
		for li, m := range r.mats {
			if m.inv.Unsupported != "" {
				return "layer " + r.img.Layers[li].DiffID + ": " + m.inv.Unsupported
			}
		}
		if r.view.unsupported != "" {
			return r.img.Ref + ": " + r.view.unsupported
		}
	}
	return ""
}

// snapshotsDirBlocks is the host cost of the snapshotter's snapshots/
// directory, one entry, named by a numeric id, per snapshot.
func snapshotsDirBlocks(n int, p Profile) int64 {
	names := make([]int, n)
	for i := range names {
		names[i] = len(strconv.Itoa(i + 1))
	}
	return ext4DirBlocks(names) * p.HostBlock
}

// overlaySize estimates the overlayfs snapshotter on ext4: each snapshot's
// upper directory holds its layer's files, with the parents its tar lacks
// created from the layers below, whiteouts as devices and opaque
// directories as an xattr. Payloads are not shared between snapshots.
func overlaySize(roots []*root, p Profile) (*Breakdown, string) {
	if why := scanUnsupported(roots); why != "" {
		return nil, why
	}
	b := &Breakdown{}
	xattrBlocks := map[string]bool{}
	snaps := snapshots(roots)
	for _, s := range snaps {
		u, why := overlaySnapshot(s.r.mats[s.li], s.r.view.implicit[s.li], p, xattrBlocks)
		if why != "" {
			return nil, why
		}
		b.PayloadAlloc += u.payloadAlloc
		b.PayloadBytes += u.payloadBytes
		b.Inodes += u.inodes
		b.Files += u.files
		b.Host += u.host
	}
	b.Host += int64(len(xattrBlocks))*p.HostBlock + snapshotsDirBlocks(len(snaps), p)
	b.Units = len(snaps)
	b.Unused = unusedFiles(roots, payloadKey, func(size int64) int64 { return alloc(size, p.HostBlock) })
	b.Reuse = snapshotReuse(roots, true)
	b.total(p)
	return b, ""
}

// overlayUnit is what one overlay snapshot takes.
type overlayUnit struct {
	payloadAlloc, payloadBytes, host, inodes, files int64
}

// overlaySnapshot sizes one upper directory. Xattr sets needing a block are
// added to xattrBlocks, which ext4 shares among identical sets.
func overlaySnapshot(m *matLayer, implicit []implicitDir, p Profile, xattrBlocks map[string]bool) (overlayUnit, string) {
	var u overlayUnit
	u.host = ext4SnapshotDirs * p.HostBlock
	dirs := map[string][]int{"/": nil}
	seen := map[int32]bool{}
	setXattrs := func(where string, xattrs []Xattr) string {
		block, fit := ext4XattrPlacement(xattrs)
		if !fit {
			return "xattrs of " + where + " exceed ext4 limits (ENOSPC without ea_inode)"
		}
		if block {
			xattrBlocks[ext4XattrKey(xattrs)] = true
		}
		return ""
	}
	for i, n := range m.names {
		if n.path != "/" {
			dirs[path.Dir(n.path)] = append(dirs[path.Dir(n.path)], len(path.Base(n.path)))
		}
		e := m.entry(i)
		if e == nil {
			u.inodes++
			continue
		}
		if seen[n.inode] {
			continue
		}
		seen[n.inode] = true
		u.inodes++
		xattrs := e.Xattrs
		switch e.Kind {
		case KindReg:
			u.files++
			u.payloadAlloc += alloc(e.Size, p.HostBlock)
			u.payloadBytes += e.Size
		case KindSymlink:
			if len(e.LinkName) > ext4FastSymlinkMax {
				u.host += p.HostBlock
			}
		case KindDir:
			if _, ok := dirs[n.path]; !ok {
				dirs[n.path] = nil
			}
			if m.opaque[n.path] {
				xattrs = append(append([]Xattr(nil), xattrs...), Xattr{Name: "trusted.overlay.opaque", Value: "y"})
			}
		}
		if why := setXattrs(n.path, xattrs); why != "" {
			return u, why
		}
	}
	for _, d := range implicit {
		u.inodes++
		dirs[path.Dir(d.path)] = append(dirs[path.Dir(d.path)], len(path.Base(d.path)))
		if _, ok := dirs[d.path]; !ok {
			dirs[d.path] = nil
		}
		xattrs := d.xattrs
		if m.opaque[d.path] {
			xattrs = append(append([]Xattr(nil), xattrs...), Xattr{Name: "trusted.overlay.opaque", Value: "y"})
		}
		if why := setXattrs(d.path, xattrs); why != "" {
			return u, why
		}
	}
	for dir, names := range dirs {
		blocks := ext4DirBlocks(names)
		if dir == "/" {
			blocks-- // fs/ is one of the snapshot's own directories
		}
		u.host += blocks * p.HostBlock
	}
	return u, ""
}

// payloadKey identifies the stored payload behind a regular-file name:
// its inode within the snapshot of chainID. ok is false for other names.
func payloadKey(chainID string, n mName, e *Entry) (key string, size int64, ok bool) {
	if e == nil || e.Kind != KindReg {
		return "", 0, false
	}
	return chainID + "/" + strconv.Itoa(int(n.inode)), e.Size, true
}

// unusedFiles counts the stored units, as key names them, that no retained
// root containing them shows under any name. A unit hidden only by
// whiteouts and opaque directories is whiteouted, any other superseded.
func unusedFiles(roots []*root, key func(chainID string, n mName, e *Entry) (string, int64, bool), allocate func(int64) int64) UnusedStats {
	used := map[string]bool{}
	hidden := map[string]bool{} // true while every hiding seen is a whiteout
	size := map[string]int64{}
	for _, r := range roots {
		for li, m := range r.mats {
			for i, n := range m.names {
				k, sz, ok := key(r.img.Layers[li].ChainID, n, m.entry(i))
				if !ok {
					continue
				}
				size[k] = sz
				switch r.view.fate[li][i] {
				case fateVisible:
					used[k] = true
				case fateSuperseded:
					hidden[k] = false
				default:
					if _, ok := hidden[k]; !ok {
						hidden[k] = true
					}
				}
			}
		}
	}
	var s UnusedStats
	for k, whiteouted := range hidden {
		if used[k] {
			continue
		}
		s.Versions++
		s.Bytes += size[k]
		s.Alloc += allocate(size[k])
		if whiteouted {
			s.Whiteouted++
		} else {
			s.Superseded++
		}
	}
	return s
}
