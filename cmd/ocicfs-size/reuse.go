// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strconv"

	"github.com/opencontainers/go-digest"
)

// Reuse explains a model's disk usage: what its store keeps once but uses
// more than once, and what it keeps more than once. A unit is what the
// store keeps per image: a layer snapshot, or a merged rootfs image. A
// content is a non-empty regular file's sha256, and every name of it is a
// use. Bytes are logical file sizes, before block rounding and fs-verity.
type Reuse struct {
	UnitsReferenced int   `json:"unitsReferenced"` // unit references summed over the images
	UnitsReused     int   `json:"unitsReused"`     // references beyond the first of each stored unit
	UnitBytes       int64 `json:"unitBytes"`       // file data of the reused references
	Contents        int   `json:"contents"`        // contents with more uses in the stored units than copies
	ContentBytes    int64 `json:"contentBytes"`    // Σ size × (uses − copies)
	Duplicated      int   `json:"duplicated"`      // contents kept in more than one copy
	ExtraCopies     int   `json:"extraCopies"`     // Σ (copies − 1)
	ExtraBytes      int64 `json:"extraBytes"`      // Σ size × (copies − 1)
}

// snapshotReuse counts reuse in a store of layer snapshots. Each stored
// snapshot keeps one copy of a content per inode when byInode is set, as
// overlay and EROFS snapshots do, or the snapshots share one copy of every
// content otherwise.
func snapshotReuse(roots []*root, byInode bool) *Reuse {
	r := &Reuse{}
	t := contentTally{}
	stored := map[string]bool{}
	for _, rt := range roots {
		for li, l := range rt.img.Layers {
			m := rt.mats[li]
			r.UnitsReferenced++
			if stored[l.ChainID] {
				r.UnitsReused++
				r.UnitBytes += layerFileBytes(m)
				continue
			}
			stored[l.ChainID] = true
			for i, n := range m.names {
				e := m.entry(i)
				if e == nil || e.Kind != KindReg || e.Size == 0 {
					continue
				}
				copyKey := ""
				if byInode {
					copyKey = l.ChainID + "/" + strconv.Itoa(int(n.inode))
				}
				t.add(e.Digest, e.Size, copyKey)
			}
		}
	}
	t.fill(r)
	return r
}

// casReuse counts reuse in the ocicfs object store: each distinct
// composefs image is stored once, keeps its own copy of every inline file
// (at most inlineMax bytes) per inode, and shares one object per content of
// every larger file with all other images.
func casReuse(roots []*casRoot, inlineMax int64) *Reuse {
	r := &Reuse{}
	t := contentTally{}
	stored := map[string]bool{}
	for _, cr := range roots {
		r.UnitsReferenced++
		if stored[cr.id] {
			r.UnitsReused++
			r.UnitBytes += rootFileBytes(cr)
			continue
		}
		stored[cr.id] = true
		inode := map[string]int{} // regular file path → index of its inode's entry
		for i, e := range cr.entries {
			switch {
			case e.Hardlink != "":
				j, ok := inode[e.Hardlink]
				if !ok {
					continue
				}
				inode[e.Path] = j
			case e.Mode.IsRegular():
				inode[e.Path] = i
			default:
				continue
			}
			le := cr.entries[inode[e.Path]]
			size := le.Size
			if le.Content != nil {
				size = int64(len(le.Content))
			}
			if size == 0 {
				continue
			}
			d := le.Digest
			if le.Content != nil {
				d = digest.FromBytes(le.Content)
			}
			copyKey := ""
			if size <= inlineMax {
				copyKey = cr.id + ":" + le.Path
			}
			t.add(d.Encoded(), size, copyKey)
		}
	}
	t.fill(r)
	return r
}

// contentUses is one content's uses and the copies a store keeps of it.
type contentUses struct {
	size   int64
	uses   int
	copies map[string]bool
}

// contentTally maps a content digest to its uses.
type contentTally map[string]*contentUses

// add records one use of content d, kept in the copy copyKey names.
func (t contentTally) add(d string, size int64, copyKey string) {
	c := t[d]
	if c == nil {
		c = &contentUses{size: size, copies: map[string]bool{}}
		t[d] = c
	}
	c.uses++
	c.copies[copyKey] = true
}

// fill adds the tallied reuse and duplication to r.
func (t contentTally) fill(r *Reuse) {
	for _, c := range t {
		k := len(c.copies)
		if extra := c.uses - k; extra > 0 {
			r.Contents++
			r.ContentBytes += c.size * int64(extra)
		}
		if k > 1 {
			r.Duplicated++
			r.ExtraCopies += k - 1
			r.ExtraBytes += c.size * int64(k-1)
		}
	}
}

// layerFileBytes is the regular-file data a layer snapshot stores, once
// per inode.
func layerFileBytes(m *matLayer) int64 {
	var n int64
	seen := map[int32]bool{}
	for i, nm := range m.names {
		if e := m.entry(i); e != nil && e.Kind == KindReg && !seen[nm.inode] {
			seen[nm.inode] = true
			n += e.Size
		}
	}
	return n
}

// rootFileBytes is the regular-file data a merged rootfs shows, once per
// inode.
func rootFileBytes(cr *casRoot) int64 {
	var n int64
	for _, e := range cr.entries {
		switch {
		case e.Hardlink != "" || !e.Mode.IsRegular():
		case e.Content != nil:
			n += int64(len(e.Content))
		default:
			n += e.Size
		}
	}
	return n
}
