// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package rootfs assembles decompressed OCI layers into a composefs image.
package rootfs

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/objectstore"
)

// MediaTypeComposefsImage is the descriptor media type for generated composefs
// image blobs stored in CAS.
const MediaTypeComposefsImage = "application/vnd.knarr.composefs.image.v1+erofs"

// Image is the generated composefs blob and its measured fs-verity digest.
type Image struct {
	Descriptor     ocispec.Descriptor
	FsverityDigest string
}

// Assemble merges decompressed layer tar streams, writes their regular file
// content to CAS, generates a composefs image, stores it in CAS, and returns its
// descriptor plus measured fs-verity digest.
func Assemble(ctx context.Context, store *objectstore.Store, layers []io.Reader, diffIDs []digest.Digest) (Image, error) {
	if store == nil {
		return Image{}, errors.New("rootfs: nil CAS store")
	}
	entries, err := buildEntries(ctx, store, layers, diffIDs)
	if err != nil {
		return Image{}, err
	}

	out, err := tempOutput(store.TempDir())
	if err != nil {
		return Image{}, err
	}
	defer func() {
		if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("rootfs: failed to remove temp file", "path", out, "err", err)
		}
	}()

	if err := composefs.WriteImage(ctx, out, entries); err != nil {
		return Image{}, fmt.Errorf("rootfs: write composefs image: %w", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return Image{}, fmt.Errorf("rootfs: stat composefs image: %w", err)
	}
	f, err := os.Open(out) // #nosec G304 -- store-owned build temp path
	if err != nil {
		return Image{}, fmt.Errorf("rootfs: open composefs image: %w", err)
	}
	d, fsv, err := store.WriteObject(ctx, f)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return Image{}, fmt.Errorf("rootfs: store composefs image: %w", err)
	}

	return Image{
		Descriptor: ocispec.Descriptor{
			MediaType: MediaTypeComposefsImage,
			Digest:    d,
			Size:      fi.Size(),
		},
		FsverityDigest: fsv,
	}, nil
}

// tempOutput reserves a unique name under dir and removes the placeholder, so
// mkcomposefs can create the image file fresh; the returned path does not yet exist.
func tempOutput(dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".composefs-*") // #nosec G304 -- store-owned temp dir
	if err != nil {
		return "", fmt.Errorf("rootfs: create composefs temp: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("rootfs: close composefs temp: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("rootfs: remove composefs temp placeholder: %w", err)
	}
	return name, nil
}

// node is one entry in the merged tree while layers are applied: its metadata,
// the content and fs-verity digests for a regular file, and the index of the
// layer that last wrote it (so an opaque marker removes only lower-layer entries).
type node struct {
	mode     os.FileMode
	uid      int
	gid      int
	xattrs   map[string]string
	linkname string
	digest   digest.Digest
	verity   digest.Digest
	size     int64
	devmajor int64
	devminor int64
	layer    int
}

// buildEntries applies every layer in order into one tree, synthesises any
// missing parent directories, and returns the merged composefs inventory.
func buildEntries(ctx context.Context, store *objectstore.Store, layers []io.Reader, diffIDs []digest.Digest) ([]composefs.Entry, error) {
	if len(layers) != len(diffIDs) {
		return nil, fmt.Errorf("rootfs: layer count %d does not match diff_id count %d", len(layers), len(diffIDs))
	}
	tree := make(map[string]*node)
	for i, id := range diffIDs {
		if layers[i] == nil {
			return nil, fmt.Errorf("rootfs: layer[%d] is nil", i)
		}
		if err := applyLayer(ctx, store, tree, layers[i], id, i); err != nil {
			return nil, err
		}
	}
	if err := ensureParents(tree); err != nil {
		return nil, err
	}
	return entriesFromTree(tree), nil
}

// applyLayer walks one decompressed layer tar into tree, then verifies the whole
// decompressed stream hashes to its diff_id at end of stream. The stream is teed
// into the hash as the tar reader consumes it, so a regular file's content is
// stored and hashed toward the diff_id in a single pass.
func applyLayer(ctx context.Context, store *objectstore.Store, tree map[string]*node, layer io.Reader, diffID digest.Digest, idx int) error {
	h := sha256.New()
	teed := io.TeeReader(&ctxReader{ctx: ctx, r: layer}, h)
	tr := tar.NewReader(teed)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("rootfs: layer %d: read tar: %w", idx, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name, err := normalizeName(hdr.Name)
		if err != nil {
			return fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		if name == "" {
			continue
		}

		dir, base := splitPath(name)
		switch {
		case base == ".wh..wh..opq":
			removeOpaque(tree, dir, idx)
			continue
		case strings.HasPrefix(base, ".wh."):
			target := strings.TrimPrefix(base, ".wh.")
			if target == "" {
				return fmt.Errorf("rootfs: layer %d: empty whiteout target %q", idx, hdr.Name)
			}
			removeSubtree(tree, path.Join(dir, target))
			continue
		}

		if err := applyHeader(ctx, store, tree, tr, hdr, name, idx); err != nil {
			return fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
	}
	if _, err := io.Copy(io.Discard, teed); err != nil {
		return fmt.Errorf("rootfs: layer %d: drain: %w", idx, err)
	}
	if got := digest.NewDigest(digest.SHA256, h); got != diffID {
		return fmt.Errorf("rootfs: layer %d diff_id mismatch: expected %s, got %s", idx, diffID, got)
	}
	return nil
}

// applyHeader merges a single tar entry into tree: a regular file streams from tr
// into CAS (recording its content and fs-verity digests), a hardlink clones its
// regular-file target, and the remaining types become metadata-only nodes.
// Whiteout and opaque markers are handled by the caller before this is reached.
func applyHeader(ctx context.Context, store *objectstore.Store, tree map[string]*node, tr *tar.Reader, hdr *tar.Header, name string, idx int) error {
	// A non-directory entry replaces whatever occupied name. If that was a
	// directory, its children lose their parent, so drop the whole subtree
	// before inserting the replacement (last-writer-wins, matching overlay
	// merge semantics). A directory entry instead merges over any existing
	// node and keeps children.
	if hdr.Typeflag != tar.TypeDir {
		if existing, ok := tree[name]; ok && existing.mode&os.ModeDir != 0 {
			removeSubtree(tree, name)
		}
	}

	switch hdr.Typeflag {
	case tar.TypeReg, '\x00': // '\x00' is the legacy regular-file flag (formerly tar.TypeRegA)
		if store == nil {
			return errors.New("regular file requires CAS store")
		}
		d, fsv, err := store.WriteObject(ctx, tr)
		if err != nil {
			return fmt.Errorf("write object for %s: %w", name, err)
		}
		verity, err := digestutil.ParseSHA256(fsv)
		if err != nil {
			return fmt.Errorf("parse fs-verity digest for %s: %w", name, err)
		}
		tree[name] = &node{
			mode:   modeFromHeader(hdr, 0),
			uid:    hdr.Uid,
			gid:    hdr.Gid,
			xattrs: xattrsFromHeader(hdr),
			digest: d,
			verity: verity,
			size:   hdr.Size,
			layer:  idx,
		}
	case tar.TypeLink:
		target, err := normalizeName(hdr.Linkname)
		if err != nil {
			return fmt.Errorf("hardlink %s target: %w", name, err)
		}
		if err := cloneHardlink(tree, name, target, idx); err != nil {
			return fmt.Errorf("hardlink %s -> %s: %w", name, target, err)
		}
	case tar.TypeSymlink:
		if hdr.Linkname == "" {
			return fmt.Errorf("symlink %s has empty target", name)
		}
		tree[name] = &node{
			mode:     modeFromHeader(hdr, os.ModeSymlink),
			uid:      hdr.Uid,
			gid:      hdr.Gid,
			xattrs:   xattrsFromHeader(hdr),
			linkname: hdr.Linkname,
			layer:    idx,
		}
	case tar.TypeDir:
		tree[name] = &node{
			mode:   modeFromHeader(hdr, os.ModeDir),
			uid:    hdr.Uid,
			gid:    hdr.Gid,
			xattrs: xattrsFromHeader(hdr),
			layer:  idx,
		}
	case tar.TypeChar:
		tree[name] = deviceNode(hdr, os.ModeDevice|os.ModeCharDevice, idx)
	case tar.TypeBlock:
		tree[name] = deviceNode(hdr, os.ModeDevice, idx)
	case tar.TypeFifo:
		tree[name] = deviceNode(hdr, os.ModeNamedPipe, idx)
	default:
		return fmt.Errorf("unsupported tar entry type %q for %s", hdr.Typeflag, name)
	}
	return nil
}

// deviceNode builds a metadata-only node for a character, block, or fifo special file.
func deviceNode(hdr *tar.Header, typ os.FileMode, idx int) *node {
	return &node{
		mode:     modeFromHeader(hdr, typ),
		uid:      hdr.Uid,
		gid:      hdr.Gid,
		xattrs:   xattrsFromHeader(hdr),
		devmajor: hdr.Devmajor,
		devminor: hdr.Devminor,
		layer:    idx,
	}
}

// normalizeName cleans a tar entry name to a root-relative path (no leading
// slash), rejecting NUL and any name that escapes the root; it returns "" for the
// root entry itself, which carries no node: any mode, ownership, or xattrs a
// layer sets on the root directory are dropped, and the generated image uses the
// default root the composefs dump synthesizes.
func normalizeName(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("path %q contains NUL", name)
	}
	if name == "." || strings.Trim(name, "/") == "" {
		return "", nil
	}

	var parts []string
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				return "", fmt.Errorf("path %q escapes root", name)
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		// The name resolves to the root directory (e.g. "." or "./") without
		// escaping it; like the root entry, it carries no node.
		return "", nil
	}
	return path.Join(parts...), nil
}

// splitPath separates a normalized path into its parent directory (without a
// trailing slash) and base name.
func splitPath(name string) (string, string) {
	dir, base := path.Split(name)
	return strings.TrimSuffix(dir, "/"), base
}

// removeSubtree deletes target and everything beneath it, implementing a whiteout.
func removeSubtree(tree map[string]*node, target string) {
	prefix := target + "/"
	for p := range tree {
		if p == target || strings.HasPrefix(p, prefix) {
			delete(tree, p)
		}
	}
}

// removeOpaque deletes the entries under dir that came from a lower layer,
// implementing an opaque-directory marker; same- and higher-layer entries stay.
func removeOpaque(tree map[string]*node, dir string, idx int) {
	prefix := dir + "/"
	for p, n := range tree {
		if dir != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		if n.layer < idx {
			delete(tree, p)
		}
	}
}

// cloneHardlink records name as an independent copy of an existing regular-file
// node, so a later whiteout of the target does not drop the link; a missing or
// non-regular target is a malformed layer.
func cloneHardlink(tree map[string]*node, name, target string, idx int) error {
	n, ok := tree[target]
	if !ok {
		return errors.New("target does not exist")
	}
	if n.mode&os.ModeType != 0 {
		return fmt.Errorf("target is not a regular file")
	}
	tree[name] = &node{
		mode:   n.mode,
		uid:    n.uid,
		gid:    n.gid,
		xattrs: cloneXattrs(n.xattrs),
		digest: n.digest,
		verity: n.verity,
		size:   n.size,
		layer:  idx,
	}
	return nil
}

// ensureParents synthesises any directory an entry needs but no layer emitted, so
// the tree satisfies composefs's parent-before-child rule; a non-directory
// ancestor is an error.
func ensureParents(tree map[string]*node) error {
	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			n, ok := tree[parent]
			if ok {
				if n.mode&os.ModeDir == 0 {
					return fmt.Errorf("rootfs: parent %s is not a directory", parent)
				}
				continue
			}
			tree[parent] = &node{mode: os.ModeDir | 0o755, layer: -1}
		}
	}
	return nil
}

// entriesFromTree converts the merged tree into composefs entries with absolute
// paths, sorted by path.
func entriesFromTree(tree map[string]*node) []composefs.Entry {
	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	entries := make([]composefs.Entry, 0, len(paths))
	for _, p := range paths {
		n := tree[p]
		entries = append(entries, composefs.Entry{
			Path:     "/" + p,
			Mode:     n.mode,
			UID:      n.uid,
			GID:      n.gid,
			Xattrs:   cloneXattrs(n.xattrs),
			Linkname: n.linkname,
			Digest:   n.digest,
			Verity:   n.verity,
			Size:     n.size,
			Devmajor: n.devmajor,
			Devminor: n.devminor,
		})
	}
	return entries
}

// modeFromHeader maps a tar header's permission and setuid/setgid/sticky bits onto
// a FileMode carrying the given type bits.
func modeFromHeader(hdr *tar.Header, typ os.FileMode) os.FileMode {
	mode := os.FileMode(hdr.Mode & 0o777)
	if hdr.Mode&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if hdr.Mode&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if hdr.Mode&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode | typ
}

// xattrsFromHeader collects the SCHILY.xattr.* records from a tar header.
func xattrsFromHeader(hdr *tar.Header) map[string]string {
	var xattrs map[string]string
	for k, v := range hdr.PAXRecords {
		if !strings.HasPrefix(k, "SCHILY.xattr.") || len(k) == len("SCHILY.xattr.") {
			continue
		}
		if xattrs == nil {
			xattrs = make(map[string]string)
		}
		xattrs[strings.TrimPrefix(k, "SCHILY.xattr.")] = v
	}
	return xattrs
}

// cloneXattrs returns an independent copy of an xattr map, or nil when it is empty.
func cloneXattrs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ctxReader aborts an in-progress read once ctx is cancelled, so a large layer
// stops promptly rather than draining fully.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}
