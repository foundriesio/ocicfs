// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

// erofsSize estimates the EROFS snapshotter on ext4: each snapshot stores
// its layer as one layer.erofs, the image mkfs.erofs builds from the layer
// tar with regular-file data in whole blocks, sealed with fs-verity only
// when the snapshotter enables it.
func erofsSize(roots []*root, p Profile) (*Breakdown, string) {
	if why := scanUnsupported(roots); why != "" {
		return nil, why
	}
	b := &Breakdown{}
	images := map[string]int64{}
	snaps := snapshots(roots)
	for _, s := range snaps {
		l := s.r.img.Layers[s.li]
		m := s.r.mats[s.li]
		f, ok := images[l.DiffID]
		if !ok {
			var err error
			if f, err = mkfsErofsImageSize(m.inv, false); err != nil {
				return nil, "layer " + l.DiffID + ": " + err.Error()
			}
			images[l.DiffID] = f
		}
		var payload, payloadBytes int64
		seen := map[int32]bool{}
		for i, n := range m.names {
			e := m.entry(i)
			if e == nil || e.Kind != KindReg || seen[n.inode] {
				continue
			}
			seen[n.inode] = true
			payload += alloc(e.Size, p.ErofsBlock)
			payloadBytes += e.Size
		}
		file := alloc(f, p.HostBlock)
		b.PayloadAlloc += payload
		b.PayloadBytes += payloadBytes
		b.Layout += file - payload
		b.ImagePad += file - f
		if p.Verity {
			b.Verity += verityBytes(f, p.VerityBlock, p.HostBlock)
		}
		b.Host += ext4SnapshotDirs * p.HostBlock
	}
	b.Host += snapshotsDirBlocks(len(snaps), p)
	b.Units = len(snaps)
	b.Files = int64(len(snaps)) // one layer.erofs per snapshot
	b.Unused = unusedFiles(roots, payloadKey, func(size int64) int64 { return alloc(size, p.ErofsBlock) })
	b.Reuse = snapshotReuse(roots, true)
	b.total(p)
	return b, ""
}

// layerCasSize estimates the layer-centric EROFS snapshotter prototype:
// each snapshot stores a metadata-only layer.erofs, built from the layer's
// headers with every regular file redirected to a content object, and the
// snapshots share one directory of fs-verity sealed objects, one per
// content digest of any stored file, hidden or not. Each snapshot's marker
// file names that directory, so it takes a block.
func layerCasSize(roots []*root, p Profile) (*Breakdown, string) {
	if why := scanUnsupported(roots); why != "" {
		return nil, why
	}
	b := &Breakdown{}
	images := map[string]int64{}
	objects := map[string]int64{}
	snaps := snapshots(roots)
	for _, s := range snaps {
		l := s.r.img.Layers[s.li]
		m := s.r.mats[s.li]
		f, ok := images[l.DiffID]
		if !ok {
			var err error
			if f, err = mkfsErofsImageSize(headerball(m.inv, p.InlineMaxLayer), true); err != nil {
				return nil, "layer " + l.DiffID + ": " + err.Error()
			}
			images[l.DiffID] = f
		}
		for i := range m.names {
			if e := m.entry(i); e != nil && e.Kind == KindReg && e.Size > p.InlineMaxLayer {
				objects[e.Digest] = e.Size
			}
		}
		file := alloc(f, p.HostBlock)
		b.Layout += file
		b.ImagePad += file - f
		if p.Verity {
			b.Verity += verityBytes(f, p.VerityBlock, p.HostBlock)
		}
		b.Host += (ext4SnapshotDirs + 1) * p.HostBlock
	}
	names := make([]string, 0, len(objects))
	for d, size := range objects {
		names = append(names, d)
		b.PayloadAlloc += alloc(size, p.HostBlock)
		b.PayloadBytes += size
		if p.Verity {
			b.Verity += verityBytes(size, p.VerityBlock, p.HostBlock)
		}
	}
	b.Host += snapshotsDirBlocks(len(snaps), p) + shardedDirBlocks(names, 0)*p.HostBlock
	b.Units = len(snaps)
	b.Objects = len(objects)
	b.Files = int64(2*len(snaps) + len(objects)) // a layer.erofs and a marker per snapshot, and the objects
	b.Unused = unusedFiles(roots, func(_ string, _ mName, e *Entry) (string, int64, bool) {
		if e == nil || e.Kind != KindReg || e.Size <= p.InlineMaxLayer {
			return "", 0, false
		}
		return e.Digest, e.Size, true
	}, func(size int64) int64 { return alloc(size, p.HostBlock) })
	b.Reuse = snapshotReuse(roots, false)
	b.total(p)
	return b, ""
}

// headerball returns the header stream of a layer as the prototype feeds it
// to mkfs.erofs: every regular file larger than inlineMax gains the overlay
// redirect to its object and a metacopy xattr carrying the object's
// fs-verity digest.
func headerball(inv *LayerInv, inlineMax int64) *LayerInv {
	out := *inv
	out.Entries = make([]Entry, len(inv.Entries))
	copy(out.Entries, inv.Entries)
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.Kind != KindReg || e.Size <= inlineMax {
			continue
		}
		e.Xattrs = append(append([]Xattr(nil), e.Xattrs...),
			Xattr{Name: "trusted.overlay.redirect", Value: "/" + e.Digest[:2] + "/" + e.Digest[2:]},
			Xattr{Name: "trusted.overlay.metacopy", Value: "\x00\x24\x00\x01" + e.Digest[:32]})
	}
	return &out
}

// shardedDirBlocks returns the host blocks of a directory sharding names by
// their first two characters, each shard holding the rest of the name plus
// suffix bytes: the top directory and every shard directory.
func shardedDirBlocks(names []string, suffix int) int64 {
	shards := map[string][]int{}
	for _, n := range names {
		shards[n[:2]] = append(shards[n[:2]], len(n)-2+suffix)
	}
	top := make([]int, 0, len(shards))
	blocks := int64(0)
	for _, lens := range shards {
		top = append(top, 2)
		blocks += ext4DirBlocks(lens)
	}
	return blocks + ext4DirBlocks(top)
}
