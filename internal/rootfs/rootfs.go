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
// RootfsSize is the apparent size of the merged rootfs: the regular-file
// content bytes of the merged tree, each hardlink group counted once. It is
// not the on-disk cost (content is shared in the object store) and not the
// sum of the layer tar sizes (overwritten and whited-out files don't count).
// Canonical reports whether the tree was assembled without any divergence from
// the upstream-reproducible construction.
type Image struct {
	Descriptor     ocispec.Descriptor
	FsverityDigest string
	RootfsSize     int64
	Canonical      bool
}

// ObjectSink records a regular file's content and returns its content digest
// and a Sealed resolving to its fs-verity digest, which may still be pending.
type ObjectSink interface {
	WriteObject(ctx context.Context, r io.Reader) (digest.Digest, objectstore.Sealed, error)
}

// Assemble merges decompressed layer tar streams, writes regular files larger
// than the inline threshold to CAS (smaller ones ride inline in the image),
// generates a composefs image, stores it in CAS, and returns its descriptor
// plus measured fs-verity digest.
//
// openLayer must return the decompressed tar stream of layer i, for i in
// [0, len(diffIDs)). It is called once per layer, in order, and at most one
// returned reader is open at a time: each is read to EOF and closed before the
// next is opened.
func Assemble(ctx context.Context, store *objectstore.Store, openLayer func(int) (io.ReadCloser, error), diffIDs []digest.Digest) (img Image, err error) {
	if store == nil {
		return Image{}, errors.New("rootfs: nil CAS store")
	}
	// Objects are sealed on the writer's background worker while the layer walk
	// goes on producing them. Defers run last-in first-out: on an error return
	// the cancel runs before the deferred Close, so the queue drains by
	// discarding its temps instead of sealing objects for a pull that has
	// already failed. The success path closes explicitly first, so every
	// object — including ones a later layer removed from the tree — is sealed
	// before the image is written. A worker failure the walk did not observe
	// is reported alongside the walk's own error rather than dropped; a
	// context error is not, being either the cancellation this error path
	// itself induces or the caller's own, which the walk already reported.
	wctx, cancel := context.WithCancel(ctx)
	w := store.NewWriter(wctx)
	defer func() {
		cerr := w.Close()
		if cerr == nil || errors.Is(err, cerr) || errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
			return
		}
		err = errors.Join(err, cerr)
	}()
	defer cancel()
	entries, canonical, err := BuildEntries(wctx, w, openLayer, diffIDs)
	if err != nil {
		return Image{}, err
	}
	if err := w.Close(); err != nil {
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
	d, sealed, err := store.WriteObject(ctx, f)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return Image{}, fmt.Errorf("rootfs: store composefs image: %w", err)
	}
	fsv, err := sealed()
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
		RootfsSize:     contentSize(entries),
		Canonical:      canonical,
	}, nil
}

// contentSize sums the regular-file content bytes of the merged tree. Only a
// payload-carrying entry has a non-zero Size, and each hardlink group's
// payload appears on exactly one entry (the others reference it by Hardlink),
// so the sum counts every inode's content once.
func contentSize(entries []composefs.Entry) int64 {
	var total int64
	for _, e := range entries {
		if e.Hardlink == "" {
			total += e.Size
		}
	}
	return total
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

// node is one name in the merged tree while layers are applied: its metadata,
// the content bytes (for a small inlined regular file) or the content digest
// and the pending fs-verity digest (for a stored one — sealed resolves it, and
// verity holds it once resolved), and the index of the
// layer that last wrote it (so an opaque marker removes only lower-layer names).
// A hardlink name carries only link and layer: link points at the node holding
// the shared inode's metadata and content, so every name of one inode reads the
// same payload, a whiteout removes just a name, and replacing one name installs
// a fresh node at it without touching the others — the semantics of link(2).
// link always points at a payload node directly, never at another link node.
type node struct {
	mode      os.FileMode
	uid       int
	gid       int
	xattrs    map[string]string
	linkname  string
	content   []byte
	digest    digest.Digest
	sealed    objectstore.Sealed
	verity    digest.Digest
	size      int64
	mtimeSec  int64
	mtimeNsec int64
	devmajor  int64
	devminor  int64
	layer     int
	link      *node
}

// BuildEntries applies every layer in order into one tree, synthesises any
// missing parent directories, applies the canonical tree transforms, and
// returns the merged composefs inventory. The bool reports whether the tree was
// built without any divergence from the upstream-reproducible construction.
func BuildEntries(ctx context.Context, sink ObjectSink, openLayer func(int) (io.ReadCloser, error), diffIDs []digest.Digest) ([]composefs.Entry, bool, error) {
	if openLayer == nil {
		return nil, false, errors.New("rootfs: nil layer opener")
	}
	tree := make(map[string]*node)
	diverged := false
	for i, id := range diffIDs {
		layer, err := openLayer(i)
		if err != nil {
			return nil, false, fmt.Errorf("rootfs: open layer %d: %w", i, err)
		}
		if layer == nil {
			return nil, false, fmt.Errorf("rootfs: layer[%d] is nil", i)
		}

		layerDiverged, applyErr := applyLayer(ctx, sink, tree, layer, id, i)
		closeErr := layer.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("rootfs: layer %d: close: %w", i, closeErr)
		}
		if applyErr != nil || closeErr != nil {
			return nil, false, errors.Join(applyErr, closeErr)
		}
		diverged = diverged || layerDiverged
	}
	if err := resolveSealed(tree); err != nil {
		return nil, false, err
	}
	synthesized, err := ensureParents(tree)
	if err != nil {
		return nil, false, err
	}
	diverged = diverged || synthesized

	root, transformDiverged := transformCanonical(tree)
	diverged = diverged || transformDiverged
	return entriesFromTree(tree, root), !diverged, nil
}

// applyLayer walks one decompressed layer tar into tree, then verifies the whole
// decompressed stream hashes to its diff_id at end of stream. The stream is teed
// into the hash as the tar reader consumes it, so a regular file's content is
// captured (stored, or read inline) and hashed toward the diff_id in one pass.
// The bool reports whether this layer diverged from the upstream-reproducible
// construction: an entry (or marker) reached a name whose immediate parent was
// absent at that moment — upstream errors there while knarr synthesises the
// parent later — or an opaque marker cleared same-layer content.
func applyLayer(ctx context.Context, sink ObjectSink, tree map[string]*node, layer io.Reader, diffID digest.Digest, idx int) (bool, error) {
	h := sha256.New()
	teed := io.TeeReader(&ctxReader{ctx: ctx, r: layer}, h)
	tr := tar.NewReader(teed)

	diverged := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("rootfs: layer %d: read tar: %w", idx, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name, err := normalizeName(hdr.Name)
		if err != nil {
			return false, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		if name == "" {
			continue
		}

		dir, base := splitPath(name)
		// Upstream resolves the parent chain before inserting a name or applying a
		// marker and errors if any component is missing or not a directory; knarr
		// instead repairs the tree after merging, so a name whose immediate parent
		// is absent — or present as a non-directory — right now is a divergence.
		// The immediate-parent check accumulates the whole chain by induction: a
		// parent inserted while absent flagged its own name.
		if dir != "" {
			if p, ok := tree[dir]; !ok || p.mode&os.ModeDir == 0 {
				diverged = true
			}
		}
		switch {
		case base == ".wh..wh..opq":
			if removeOpaque(tree, dir, idx) {
				diverged = true
			}
			continue
		case strings.HasPrefix(base, ".wh."):
			target := strings.TrimPrefix(base, ".wh.")
			if target == "" {
				return false, fmt.Errorf("rootfs: layer %d: empty whiteout target %q", idx, hdr.Name)
			}
			removeSubtree(tree, path.Join(dir, target))
			continue
		}

		if err := applyHeader(ctx, sink, tree, tr, hdr, name, idx); err != nil {
			return false, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
	}
	if _, err := io.Copy(io.Discard, teed); err != nil {
		return false, fmt.Errorf("rootfs: layer %d: drain: %w", idx, err)
	}
	if got := digest.NewDigest(digest.SHA256, h); got != diffID {
		return false, fmt.Errorf("rootfs: layer %d diff_id mismatch: expected %s, got %s", idx, diffID, got)
	}
	return diverged, nil
}

// applyHeader merges a single tar entry into tree: a regular file of at most
// composefs.InlineContentMax bytes has its bytes read inline, a larger one
// streams from tr into the object sink (recording its content and fs-verity
// digests), a hardlink becomes another name of its target's inode, and the
// remaining types become metadata-only nodes. Every node type carries the tar
// entry's modification time. Whiteout and opaque markers are handled by the
// caller before this is reached.
func applyHeader(ctx context.Context, sink ObjectSink, tree map[string]*node, tr *tar.Reader, hdr *tar.Header, name string, idx int) error {
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

	sec, nsec := mtimeFromHeader(hdr)
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00': // '\x00' is the legacy regular-file flag (formerly tar.TypeRegA)
		if hdr.Size <= composefs.InlineContentMax {
			// A small file is inlined into the EROFS metadata: read its bytes (the
			// tar reader caps the read at the entry size) and skip the object store.
			content, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("read inline content for %s: %w", name, err)
			}
			tree[name] = &node{
				mode:      modeFromHeader(hdr, 0),
				uid:       hdr.Uid,
				gid:       hdr.Gid,
				xattrs:    xattrsFromHeader(hdr),
				content:   content,
				size:      int64(len(content)),
				mtimeSec:  sec,
				mtimeNsec: nsec,
				layer:     idx,
			}
			return nil
		}
		if sink == nil {
			return errors.New("regular file requires object sink")
		}
		d, sealed, err := sink.WriteObject(ctx, tr)
		if err != nil {
			return fmt.Errorf("write object for %s: %w", name, err)
		}
		tree[name] = &node{
			mode:      modeFromHeader(hdr, 0),
			uid:       hdr.Uid,
			gid:       hdr.Gid,
			xattrs:    xattrsFromHeader(hdr),
			digest:    d,
			sealed:    sealed,
			size:      hdr.Size,
			mtimeSec:  sec,
			mtimeNsec: nsec,
			layer:     idx,
		}
	case tar.TypeLink:
		target, err := normalizeName(hdr.Linkname)
		if err != nil {
			return fmt.Errorf("hardlink %s target: %w", name, err)
		}
		if err := linkHardlink(tree, name, target, idx); err != nil {
			return fmt.Errorf("hardlink %s -> %s: %w", name, target, err)
		}
	case tar.TypeSymlink:
		if hdr.Linkname == "" {
			return fmt.Errorf("symlink %s has empty target", name)
		}
		tree[name] = &node{
			mode:      modeFromHeader(hdr, os.ModeSymlink),
			uid:       hdr.Uid,
			gid:       hdr.Gid,
			xattrs:    xattrsFromHeader(hdr),
			linkname:  hdr.Linkname,
			mtimeSec:  sec,
			mtimeNsec: nsec,
			layer:     idx,
		}
	case tar.TypeDir:
		tree[name] = &node{
			mode:      modeFromHeader(hdr, os.ModeDir),
			uid:       hdr.Uid,
			gid:       hdr.Gid,
			xattrs:    xattrsFromHeader(hdr),
			mtimeSec:  sec,
			mtimeNsec: nsec,
			layer:     idx,
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
	sec, nsec := mtimeFromHeader(hdr)
	return &node{
		mode:      modeFromHeader(hdr, typ),
		uid:       hdr.Uid,
		gid:       hdr.Gid,
		xattrs:    xattrsFromHeader(hdr),
		mtimeSec:  sec,
		mtimeNsec: nsec,
		devmajor:  hdr.Devmajor,
		devminor:  hdr.Devminor,
		layer:     idx,
	}
}

// mtimeFromHeader extracts a tar header's modification time as whole seconds and
// a nanosecond remainder in [0, 1e9). Times before the epoch — including the
// zero-value time a header without an mtime decodes to — clamp to 0, since the
// composefs-dump parser reads the MTIME column as an unsigned value.
func mtimeFromHeader(hdr *tar.Header) (int64, int64) {
	sec := hdr.ModTime.Unix()
	if sec < 0 {
		return 0, 0
	}
	return sec, int64(hdr.ModTime.Nanosecond())
}

// normalizeName cleans a tar entry name to a root-relative path (no leading
// slash), rejecting NUL and any name that escapes the root; it returns "" for the
// root entry itself, which carries no node: any mode, ownership, or xattrs a
// layer sets on the root directory are dropped, since the canonical transform
// derives the root's metadata from the metadata source instead.
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

// resolveSealed fills in the fs-verity digest of every stored payload node once
// its object is sealed, waiting as needed. Payloads are reached through
// hardlink wrappers as well as by name: a payload whose own name was whited
// out survives only via link. Each payload is resolved once.
func resolveSealed(tree map[string]*node) error {
	for name, n := range tree {
		if n.link != nil {
			n = n.link
		}
		if n.sealed == nil {
			continue
		}
		fsv, err := n.sealed()
		if err != nil {
			return fmt.Errorf("rootfs: seal object for %s: %w", name, err)
		}
		verity, err := digestutil.ParseSHA256(fsv)
		if err != nil {
			return fmt.Errorf("rootfs: parse fs-verity digest for %s: %w", name, err)
		}
		n.verity = verity
		n.sealed = nil
	}
	return nil
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

// removeChildren deletes everything beneath dir while keeping the dir node,
// emptying a directory in place.
func removeChildren(tree map[string]*node, dir string) {
	prefix := dir + "/"
	for p := range tree {
		if strings.HasPrefix(p, prefix) {
			delete(tree, p)
		}
	}
}

// removeOpaque deletes the entries under dir that came from a lower layer,
// implementing an opaque-directory marker; same- and higher-layer entries stay.
// It reports whether any same-layer entry already sits under dir at this point:
// the upstream construction clears the whole directory on the marker and so
// drops such entries, while knarr keeps them per OCI layer semantics — a
// divergence.
func removeOpaque(tree map[string]*node, dir string, idx int) bool {
	prefix := dir + "/"
	sameLayer := false
	for p, n := range tree {
		if dir != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		switch {
		case n.layer < idx:
			delete(tree, p)
		case n.layer == idx:
			sameLayer = true
		}
	}
	return sameLayer
}

// linkHardlink records name as another name of target's inode: the new name's
// node points at the payload node all names of that inode share, so the final
// image gives them one inode, exactly as link(2) would. A missing target is a
// malformed layer, and a directory target is rejected as it is by every kernel;
// any other type — including a symlink or device — can be hardlinked.
func linkHardlink(tree map[string]*node, name, target string, idx int) error {
	n, ok := tree[target]
	if !ok {
		return errors.New("target does not exist")
	}
	if n.mode&os.ModeDir != 0 {
		return errors.New("target is a directory")
	}
	if n.link != nil {
		n = n.link
	}
	tree[name] = &node{layer: idx, link: n}
	return nil
}

// ensureParents synthesises any directory an entry needs but no layer emitted, so
// the tree satisfies composefs's parent-before-child rule; a non-directory
// ancestor is an error. It reports whether it invented any parent: the upstream
// construction errors on a missing parent rather than synthesising one, so an
// invented parent is a divergence.
func ensureParents(tree map[string]*node) (bool, error) {
	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	synthesized := false
	for _, p := range paths {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			n, ok := tree[parent]
			if ok {
				if n.mode&os.ModeDir == 0 {
					return false, fmt.Errorf("rootfs: parent %s is not a directory", parent)
				}
				continue
			}
			tree[parent] = &node{mode: os.ModeDir | 0o755, layer: -1}
			synthesized = true
		}
	}
	return synthesized, nil
}

// canonicalXattrKey is the only extended attribute the canonical construction
// keeps; every other xattr is dropped from every node.
const canonicalXattrKey = "security.capability"

// transformCanonical normalises the merged tree for the canonical construction
// and returns the root directory node together with whether the tree diverged
// from the upstream-reproducible form. It derives the root's metadata from the
// metadata source, empties /run in place while keeping the directory node, and
// drops from every node all xattrs but security.capability.
//
// The metadata source is /usr's metadata when /usr is a directory; otherwise the
// canonical default {mode 0755, uid 0, gid 0, mtime 0, no xattrs}, and that
// substitution is a divergence. A /run that exists but is not a directory is
// left untouched and is likewise a divergence.
func transformCanonical(tree map[string]*node) (*node, bool) {
	diverged := false
	src := node{mode: os.ModeDir | 0o755}
	if usr, ok := tree["usr"]; ok && usr.mode&os.ModeDir != 0 {
		src = *usr
	} else {
		diverged = true
	}

	root := &node{
		mode:      src.mode,
		uid:       src.uid,
		gid:       src.gid,
		xattrs:    cloneXattrs(src.xattrs),
		mtimeSec:  src.mtimeSec,
		mtimeNsec: src.mtimeNsec,
		layer:     -1,
	}

	if run, ok := tree["run"]; ok {
		if run.mode&os.ModeDir != 0 {
			removeChildren(tree, "run")
			run.mtimeSec = src.mtimeSec
			run.mtimeNsec = src.mtimeNsec
		} else {
			diverged = true
		}
	}

	// Filter each inode once. A hardlink wrapper carries no xattrs of its own; its
	// payload node is reached through link and may no longer sit in the tree under
	// a name of its own (its original name could have been whited out), so it must
	// be filtered via the wrapper rather than only by map iteration.
	seen := make(map[*node]bool)
	filter := func(n *node) {
		if seen[n] {
			return
		}
		seen[n] = true
		filterXattrs(n)
	}
	filter(root)
	for _, n := range tree {
		if n.link != nil {
			n = n.link
		}
		filter(n)
	}
	return root, diverged
}

// entriesFromTree converts the merged tree into composefs entries with absolute
// paths, sorted by path. The root node leads as the "/" entry. Names sharing an
// inode collapse to one full entry — the first surviving name in path order
// carries the payload and the inode's name count — with every further name
// emitted as a hardlink entry referencing it, so the target of a hardlink entry
// always precedes it in the sorted dump.
func entriesFromTree(tree map[string]*node, root *node) []composefs.Entry {
	paths := make([]string, 0, len(tree))
	nlink := make(map[*node]int, len(tree))
	for p, n := range tree {
		paths = append(paths, p)
		if n.link != nil {
			n = n.link
		}
		if n.mode&os.ModeDir == 0 {
			nlink[n]++
		}
	}
	sort.Strings(paths)
	entries := make([]composefs.Entry, 0, len(paths)+1)
	entries = append(entries, composefs.Entry{
		Path:      "/",
		Mode:      root.mode,
		UID:       root.uid,
		GID:       root.gid,
		Xattrs:    cloneXattrs(root.xattrs),
		Mtime:     root.mtimeSec,
		MtimeNsec: root.mtimeNsec,
	})
	first := make(map[*node]string, len(nlink))
	for _, p := range paths {
		n := tree[p]
		if n.link != nil {
			n = n.link
		}
		if n.mode&os.ModeDir == 0 {
			if target, ok := first[n]; ok {
				entries = append(entries, composefs.Entry{
					Path:     "/" + p,
					Hardlink: "/" + target,
				})
				continue
			}
			first[n] = p
		}
		entries = append(entries, composefs.Entry{
			Path:      "/" + p,
			Mode:      n.mode,
			UID:       n.uid,
			GID:       n.gid,
			Xattrs:    cloneXattrs(n.xattrs),
			Linkname:  n.linkname,
			Content:   n.content,
			Digest:    n.digest,
			Verity:    n.verity,
			Size:      n.size,
			Mtime:     n.mtimeSec,
			MtimeNsec: n.mtimeNsec,
			Devmajor:  n.devmajor,
			Devminor:  n.devminor,
			Nlink:     nlink[n],
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

// filterXattrs drops every xattr on a node but security.capability.
func filterXattrs(n *node) {
	if len(n.xattrs) == 0 {
		return
	}
	if v, ok := n.xattrs[canonicalXattrKey]; ok {
		n.xattrs = map[string]string{canonicalXattrKey: v}
	} else {
		n.xattrs = nil
	}
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
