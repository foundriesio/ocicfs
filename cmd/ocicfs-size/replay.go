// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path"
	"sort"
	"strings"
)

// root is one retained image: its layers as their snapshots store them and
// the replay of the chain.
type root struct {
	img  *Image
	mats []*matLayer
	view *rootView
}

// matLayer is what a layer snapshot stores once its tar is applied on its
// own: every name that survives later names of the same tar, the inode
// each one is, and the directories marked opaque.
type matLayer struct {
	inv    *LayerInv
	names  []mName
	opaque map[string]bool
}

// mName is one surviving name; inode indexes the entry holding the inode's
// metadata (a hardlink's target), or is -1 for a whiteout.
type mName struct {
	path  string
	inode int32
}

// materialize applies a layer's entries in stream order: a later name
// replaces an earlier one, and everything beneath it, unless both are
// directories, when the later one's metadata wins. A whiteout is a name of
// its own here, as the device the snapshot stores.
func materialize(inv *LayerInv) *matLayer {
	m := &matLayer{inv: inv, opaque: map[string]bool{}}
	at := map[string]int{}
	for i, e := range inv.Entries {
		if e.Kind == KindOpaque {
			m.opaque[e.Path] = true
			continue
		}
		inode := int32(i)
		switch e.Kind {
		case KindWhiteout:
			inode = -1
		case KindHardlink:
			inode = e.Link
		}
		if j, ok := at[e.Path]; ok {
			old := m.names[j]
			if old.inode >= 0 && inv.Entries[old.inode].Kind == KindDir && e.Kind == KindDir {
				m.names[j].inode = inode
				continue
			}
			m.names[j].path = ""
			delete(at, e.Path)
			prefix := e.Path + "/"
			for k := range m.names {
				if strings.HasPrefix(m.names[k].path, prefix) {
					delete(at, m.names[k].path)
					m.names[k].path = ""
				}
			}
		}
		at[e.Path] = len(m.names)
		m.names = append(m.names, mName{path: e.Path, inode: inode})
	}
	kept := m.names[:0]
	for _, n := range m.names {
		if n.path != "" {
			kept = append(kept, n)
		}
	}
	m.names = kept
	return m
}

// entry returns the inode entry of name i, or nil for a whiteout or a
// hardlink to another layer.
func (m *matLayer) entry(i int) *Entry {
	if m.names[i].inode < 0 {
		return nil
	}
	return &m.inv.Entries[m.names[i].inode]
}

// The fate of a stored name in one root.
const (
	fateVisible uint8 = iota
	fateWhiteouted
	fateSuperseded
)

// rootView is the replay of one image's layers: the fate of every stored
// name, the directories each layer's snapshot creates for names whose
// parents its tar lacks, and why the layer-scan models cannot size it.
type rootView struct {
	fate        [][]uint8
	implicit    [][]implicitDir
	unsupported string
}

// implicitDir is a directory a layer snapshot gets without its tar naming
// it, with the xattrs it copies from the layers below.
type implicitDir struct {
	path   string
	xattrs []Xattr
}

// tnode is a name of the merged tree: the layer and name providing it, or
// layer -1 for a directory no layer names.
type tnode struct {
	layer, name int
	dir         bool
	symlink     bool
	xattrs      []Xattr
	children    map[string]*tnode
}

// replay applies an image's layers bottom-up. Each layer first applies its
// whiteouts and opaque markers to the layers below, then installs its
// names: one replacing a directory with a directory keeps the children,
// any other replacement drops the old subtree.
func replay(layers []*matLayer) *rootView {
	v := &rootView{fate: make([][]uint8, len(layers)), implicit: make([][]implicitDir, len(layers))}
	root := &tnode{layer: -1, dir: true, children: map[string]*tnode{}}
	for li, m := range layers {
		v.fate[li] = make([]uint8, len(m.names))
		for _, e := range m.inv.Entries {
			switch e.Kind {
			case KindWhiteout:
				if parent, n := lookup(root, e.Path); n != nil {
					delete(parent.children, path.Base(e.Path))
					v.mark(n, fateWhiteouted)
				}
			case KindOpaque:
				if _, n := lookup(root, e.Path); n != nil && n.dir {
					for name, c := range n.children {
						delete(n.children, name)
						v.mark(c, fateWhiteouted)
					}
				}
			}
		}
		v.implicit[li] = v.implicitDirs(root, m)
		for i, n := range m.names {
			if n.inode < 0 || n.path == "/" {
				continue
			}
			v.install(root, layers, li, i)
		}
	}
	return v
}

// implicitDirs returns the directories layer m's snapshot has to create:
// parents of its names that its tar does not name, each with the xattrs of
// the directory the layers below have there. It records a name beneath a
// symlink, which this replay does not follow, as unsupported.
func (v *rootView) implicitDirs(root *tnode, m *matLayer) []implicitDir {
	named := map[string]int{}
	for i, n := range m.names {
		named[n.path] = i
	}
	paths := make([]string, 0, len(m.names)+len(m.opaque))
	for _, n := range m.names {
		paths = append(paths, n.path)
	}
	for dir := range m.opaque {
		paths = append(paths, path.Join(dir, ".wh..wh..opq"))
	}
	seen := map[string]bool{}
	var out []implicitDir
	for _, p := range paths {
		for dir := path.Dir(p); dir != "/" && p != "/"; dir = path.Dir(dir) {
			if i, ok := named[dir]; ok {
				if e := m.entry(i); e != nil && e.Kind == KindSymlink && v.unsupported == "" {
					v.unsupported = "entry " + p + " beneath symlink " + dir
				}
				continue
			}
			if _, low := lookup(root, dir); low != nil && low.symlink && v.unsupported == "" {
				v.unsupported = "entry " + p + " beneath symlink " + dir
			}
			if seen[dir] {
				continue
			}
			seen[dir] = true
			d := implicitDir{path: dir}
			if _, low := lookup(root, dir); low != nil && low.dir {
				d.xattrs = low.xattrs
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// install puts name i of layer li into the tree.
func (v *rootView) install(root *tnode, layers []*matLayer, li, i int) {
	m := layers[li]
	p := m.names[i].path
	parent := root
	for _, part := range strings.Split(strings.TrimPrefix(path.Dir(p), "/"), "/") {
		if part == "" {
			continue
		}
		c := parent.children[part]
		if c == nil || !c.dir {
			if c != nil {
				v.mark(c, fateSuperseded)
			}
			c = &tnode{layer: -1, dir: true, children: map[string]*tnode{}}
			parent.children[part] = c
		}
		parent = c
	}
	e := m.entry(i)
	n := &tnode{layer: li, name: i, dir: e.Kind == KindDir, symlink: e.Kind == KindSymlink, xattrs: e.Xattrs}
	base := path.Base(p)
	if old := parent.children[base]; old != nil {
		if old.dir && n.dir {
			if old.layer >= 0 {
				v.fate[old.layer][old.name] = fateSuperseded
			}
			n.children = old.children
		} else {
			v.mark(old, fateSuperseded)
		}
	}
	if n.dir && n.children == nil {
		n.children = map[string]*tnode{}
	}
	parent.children[base] = n
}

// mark gives a removed subtree a fate.
func (v *rootView) mark(n *tnode, fate uint8) {
	if n.layer >= 0 {
		v.fate[n.layer][n.name] = fate
	}
	for _, c := range n.children {
		v.mark(c, fate)
	}
}

// lookup returns the node at p and its parent, or a nil node.
func lookup(root *tnode, p string) (*tnode, *tnode) {
	parent, n := root, root
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == "" {
			continue
		}
		if !n.dir {
			return nil, nil
		}
		parent, n = n, n.children[part]
		if n == nil {
			return nil, nil
		}
	}
	return parent, n
}
