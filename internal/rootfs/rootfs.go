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
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/digestutil"
	"github.com/foundriesio/ocicfs/internal/objectstore"
)

// MediaTypeComposefsImage is the descriptor media type for generated composefs
// image blobs stored in CAS.
const MediaTypeComposefsImage = "application/vnd.ocicfs.composefs.image.v1+erofs"

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

// Builder assembles one composefs image from layers handed to it one at a
// time. A scanned layer arrives complete: every stored object of it is sealed
// and its fs-verity digest resolved before the Scan that produced it returns.
// A layer made from a table of contents, or decoded from a saved layer,
// arrives with its payloads pending; once every layer is merged, Missing
// resolves the payloads the store already holds and names the rest, and
// Supply takes their bytes. From Missing until Finish or Close the builder
// owns a sealing writer bound to the context Missing was given. Merge,
// Missing, Finish and Close are called from one goroutine, in that order, and
// none of them overlaps a Supply; Supply may be called from several
// goroutines at once between Missing and Finish, each payload from one of
// them.
type Builder struct {
	store *objectstore.Store
	m     *merge
	next  int  // index of the next layer Merge expects
	spent bool // Finish or Close has run, or a Merge failed

	w      *objectstore.Writer // seals supplied payloads; open from Missing until Finish or Close
	cancel context.CancelFunc  // cancels w's context

	mu      sync.Mutex                // guards pending against concurrent Supply calls
	pending map[digest.Digest][]*node // the payloads Missing returned that Supply has not seen yet
}

// Payload is a regular file the merged tree references and the store lacks.
// One of at most composefs.InlineContentMax bytes is inlined into the image;
// a larger one is stored as an object.
type Payload struct {
	Digest digest.Digest
	Size   int64
}

// errBuilderSpent reports a call on a Builder that has finished, been closed,
// or failed a Merge.
var errBuilderSpent = errors.New("rootfs: builder is spent")

// NewBuilder returns a Builder that stores objects and the image in store.
func NewBuilder(store *objectstore.Store) *Builder {
	return &Builder{store: store, m: newMerge()}
}

// Scan reads layer idx's decompressed tar stream and returns its entries, every
// stored payload carrying its content and fs-verity digests. diffID is verified
// at end of stream. Objects are staged on the calling goroutine and sealed by a
// writer opened for this call and closed before it returns. Scan is
// independent of the builder's lifetime: it touches no builder state but the
// store, so scans of different layers may run concurrently, and one may run
// after Close or Finish, when its Layer can no longer be merged; Merge, which
// applies layers in index order, Missing, Supply and Finish refuse a spent
// builder.
func (b *Builder) Scan(ctx context.Context, layer io.Reader, diffID digest.Digest, idx int) (_ *Layer, err error) {
	// Objects are sealed on the writer's background worker while the layer walk
	// goes on producing them. Defers run last-in first-out: on an error return
	// the cancel runs before the deferred Close, so the queue drains by
	// discarding its temps instead of sealing objects for a layer that has
	// already failed. The success path closes explicitly first, so every
	// object — including ones a later merge will drop from the tree — is
	// sealed before its digest is resolved. A worker failure the walk did not
	// observe is reported alongside the walk's own error rather than dropped;
	// a context error is not, being either the cancellation this error path
	// itself induces or the caller's own, which the walk already reported.
	wctx, cancel := context.WithCancel(ctx)
	w := b.store.NewWriter(wctx)
	defer func() {
		cerr := w.Close()
		if cerr == nil || errors.Is(err, cerr) || errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
			return
		}
		err = errors.Join(err, cerr)
	}()
	defer cancel()
	l, err := scanLayer(wctx, w, layer, diffID, idx)
	if err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := l.resolve(); err != nil {
		return nil, err
	}
	return l, nil
}

// Merge applies a scanned layer into the tree. Layers merge in index order: a
// layer whose index is not the next expected one is refused, and that refusal,
// like a failed merge, spends the builder.
func (b *Builder) Merge(l *Layer) error {
	if b.spent {
		return errBuilderSpent
	}
	if b.w != nil {
		return errors.New("rootfs: Merge called after Missing")
	}
	if l.idx != b.next {
		b.spent = true
		return fmt.Errorf("rootfs: merge layer %d, expected layer %d", l.idx, b.next)
	}
	if err := b.m.merge(l); err != nil {
		b.spent = true
		return err
	}
	b.next++
	return nil
}

// Missing resolves every pending stored payload the store already holds and
// returns the rest, one per unique digest, in path order of the merged tree.
// Only payloads that survived the merge are considered. An inline payload is
// always returned, never resolved from the store: its bytes belong in the
// image, and the same bytes held as some object do not put them there.
// Resolving a held object also ensures its view link. Missing is called once,
// after the last Merge; it opens the writer that seals supplied payloads under
// ctx.
func (b *Builder) Missing(ctx context.Context) ([]Payload, error) {
	if b.spent {
		return nil, errBuilderSpent
	}
	if b.w != nil {
		return nil, errors.New("rootfs: Missing called twice")
	}
	paths := make([]string, 0, len(b.m.tree))
	for p := range b.m.tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// A payload node is reached through every name of its inode, and may have
	// lost its own name to a whiteout, so nodes are deduplicated by identity.
	seen := make(map[*node]bool)
	pending := make(map[digest.Digest][]*node)
	var order []Payload
	for _, p := range paths {
		n := b.m.tree[p]
		if n.link != nil {
			n = n.link
		}
		if seen[n] || n.digest == "" || n.verity != "" {
			continue
		}
		seen[n] = true
		if nodes := pending[n.digest]; len(nodes) == 0 {
			order = append(order, Payload{Digest: n.digest, Size: n.size})
		} else if nodes[0].size != n.size {
			return nil, fmt.Errorf("rootfs: payload %s is listed with sizes %d and %d", n.digest, nodes[0].size, n.size)
		}
		pending[n.digest] = append(pending[n.digest], n)
	}

	var missing []Payload
	for _, p := range order {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("rootfs: %w", err)
		}
		if p.Size <= composefs.InlineContentMax {
			missing = append(missing, p)
			continue
		}
		fsv, size, err := b.store.ResolveObject(p.Digest)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, p)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("rootfs: resolve payload %s: %w", p.Digest, err)
		}
		// The size goes into the image, so it must be the object's.
		if size != p.Size {
			return nil, fmt.Errorf("rootfs: payload %s is listed with %d bytes but the stored object has %d", p.Digest, p.Size, size)
		}
		verity, err := digestutil.ParseSHA256(fsv)
		if err != nil {
			return nil, fmt.Errorf("rootfs: parse fs-verity digest for %s: %w", p.Digest, err)
		}
		for _, n := range pending[p.Digest] {
			n.verity = verity
		}
		delete(pending, p.Digest)
	}

	wctx, cancel := context.WithCancel(ctx)
	b.w, b.cancel, b.pending = b.store.NewWriter(wctx), cancel, pending
	return missing, nil
}

// Supply hands the builder the uncompressed bytes of p, a payload Missing
// returned. The bytes must hash to p.Digest. A stored payload is staged on the
// calling goroutine and sealed in the background; an inline payload must be
// exactly p.Size bytes and becomes the content of every file that names it. A
// stored payload of another size is refused as well, though the store keeps
// its object, which is the one its digest names. A failed Supply leaves p
// missing, so it may be supplied again, unless sealing failed: that fails every
// later Supply of a stored payload, and Finish. Supplies of distinct payloads
// may run concurrently: each touches only its own payload's nodes.
func (b *Builder) Supply(ctx context.Context, p Payload, r io.Reader) error {
	if b.spent {
		return errBuilderSpent
	}
	b.mu.Lock()
	nodes := b.pending[p.Digest]
	b.mu.Unlock()
	if len(nodes) == 0 || nodes[0].size != p.Size {
		return fmt.Errorf("rootfs: payload %s of %d bytes is not missing", p.Digest, p.Size)
	}
	if p.Size <= composefs.InlineContentMax {
		// One byte past the size tells a longer stream from an exact one.
		content, err := io.ReadAll(io.LimitReader(&ctxReader{ctx: ctx, r: r}, p.Size+1))
		if err != nil {
			return fmt.Errorf("rootfs: read inline payload %s: %w", p.Digest, err)
		}
		if int64(len(content)) != p.Size {
			return fmt.Errorf("rootfs: inline payload %s is not the expected %d bytes", p.Digest, p.Size)
		}
		if got := digest.FromBytes(content); got != p.Digest {
			return fmt.Errorf("rootfs: inline payload digest mismatch: expected %s, got %s", p.Digest, got)
		}
		for _, n := range nodes {
			n.content = content
			n.digest = ""
		}
	} else {
		counted := &countReader{r: &ctxReader{ctx: ctx, r: r}}
		sealed, err := b.w.WriteExpected(ctx, p.Digest, counted)
		if err != nil {
			return fmt.Errorf("rootfs: write payload %s: %w", p.Digest, err)
		}
		// The bytes are the object the digest names, so the store keeps it, but
		// the size that would go into the image is not the object's.
		if counted.n != p.Size {
			return fmt.Errorf("rootfs: payload %s is listed with %d bytes but has %d", p.Digest, p.Size, counted.n)
		}
		for _, n := range nodes {
			n.sealed = sealed
		}
	}
	b.mu.Lock()
	delete(b.pending, p.Digest)
	b.mu.Unlock()
	return nil
}

// Finish assembles the merged entries, writes the composefs image, stores it
// in CAS, and returns its descriptor plus measured fs-verity digest. It first
// waits for every supplied payload to be sealed; a pending payload that was
// never supplied is an error. The builder is spent afterwards.
func (b *Builder) Finish(ctx context.Context) (Image, error) {
	if b.spent {
		return Image{}, errBuilderSpent
	}
	b.spent = true
	if err := b.closeWriter(); err != nil {
		return Image{}, err
	}
	if err := b.m.resolve(); err != nil {
		return Image{}, err
	}
	entries, canonical, err := b.m.entries()
	if err != nil {
		return Image{}, err
	}
	return storeImage(ctx, b.store, entries, canonical)
}

// Close marks a build that will not finish as spent: Merge, Missing, Supply
// and Finish refuse it from then on; Scan does not depend on it. If Missing
// opened the sealing writer, Close cancels it, so payloads still queued are
// discarded rather than sealed, and waits for its worker to stop: no goroutine
// and no staged temp outlives Close. Idempotent; a no-op after Finish.
func (b *Builder) Close() {
	b.spent = true
	if b.w == nil {
		return
	}
	b.cancel()
	err := b.closeWriter()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("rootfs: abandoned build failed to seal a payload", "err", err)
	}
}

// closeWriter closes the sealing writer, if Missing opened one and it is still
// open, waiting for every queued payload to be sealed or discarded, and
// returns the first error the batch hit.
func (b *Builder) closeWriter() error {
	if b.w == nil {
		return nil
	}
	err := b.w.Close()
	b.cancel()
	b.w = nil
	return err
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
func Assemble(ctx context.Context, store *objectstore.Store, openLayer func(int) (io.ReadCloser, error), diffIDs []digest.Digest) (Image, error) {
	if store == nil {
		return Image{}, errors.New("rootfs: nil CAS store")
	}
	if openLayer == nil {
		return Image{}, errors.New("rootfs: nil layer opener")
	}
	b := NewBuilder(store)
	defer b.Close()
	for i, id := range diffIDs {
		err := applyOpened(openLayer, i, func(layer io.Reader) error {
			l, err := b.Scan(ctx, layer, id, i)
			if err != nil {
				return err
			}
			return b.Merge(l)
		})
		if err != nil {
			return Image{}, err
		}
	}
	return b.Finish(ctx)
}

// storeImage writes the composefs image for entries into a store temp, stores
// it as a CAS object, and returns its descriptor plus measured fs-verity
// digest.
func storeImage(ctx context.Context, store *objectstore.Store, entries []composefs.Entry, canonical bool) (Image, error) {
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
// and the fs-verity digest (for a stored one — sealed resolves it while it is
// pending, and verity holds it once resolved), and the index of the
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

// BuildEntries scans and merges every layer in order into one tree,
// synthesises any missing parent directories, applies the canonical tree
// transforms, and returns the merged composefs inventory. The bool reports
// whether the tree was built without any divergence from the
// upstream-reproducible construction.
func BuildEntries(ctx context.Context, sink ObjectSink, openLayer func(int) (io.ReadCloser, error), diffIDs []digest.Digest) ([]composefs.Entry, bool, error) {
	if openLayer == nil {
		return nil, false, errors.New("rootfs: nil layer opener")
	}
	m := newMerge()
	for i, id := range diffIDs {
		var l *Layer
		err := applyOpened(openLayer, i, func(layer io.Reader) error {
			var scanErr error
			l, scanErr = scanLayer(ctx, sink, layer, id, i)
			return scanErr
		})
		if err != nil {
			return nil, false, err
		}
		if err := l.resolve(); err != nil {
			return nil, false, err
		}
		if err := m.merge(l); err != nil {
			return nil, false, err
		}
	}
	return m.entries()
}

// merge is one image's layers being applied in order into one tree. diverged
// records whether any layer so far diverged from the upstream-reproducible
// construction.
type merge struct {
	tree     map[string]*node
	diverged bool
}

// newMerge returns an empty merge no layer has been applied to yet.
func newMerge() *merge {
	return &merge{tree: make(map[string]*node)}
}

// merge replays a scanned layer's entries into the tree in stream order.
func (m *merge) merge(l *Layer) error {
	for _, rec := range l.records {
		if err := m.mergeRecord(rec, l.idx); err != nil {
			return err
		}
	}
	return nil
}

// mergeRecord merges one entry of layer idx into the tree and accumulates
// whether it diverged.
func (m *merge) mergeRecord(rec record, idx int) error {
	diverged, err := mergeRecord(m.tree, rec, idx)
	m.diverged = m.diverged || diverged
	return err
}

// resolve waits for every payload in the tree that is still being sealed and
// records its fs-verity digest. A payload with neither content nor an
// fs-verity digest afterwards was never supplied, which is an error.
func (m *merge) resolve() error {
	for name, n := range m.tree {
		if n.link != nil {
			n = n.link
		}
		if n.sealed != nil {
			if err := resolveNode(name, n); err != nil {
				return err
			}
		}
		if n.digest != "" && n.verity == "" {
			return fmt.Errorf("rootfs: payload %s of %s was never supplied", n.digest, name)
		}
	}
	return nil
}

// entries synthesises any missing parent directories, applies the canonical
// tree transforms, and returns the merged composefs inventory. The bool
// reports whether the tree was built without any divergence. Every stored
// payload's fs-verity digest is resolved by then.
func (m *merge) entries() ([]composefs.Entry, bool, error) {
	synthesized, err := ensureParents(m.tree)
	if err != nil {
		return nil, false, err
	}
	diverged := m.diverged || synthesized

	root, transformDiverged := transformCanonical(m.tree)
	diverged = diverged || transformDiverged
	return entriesFromTree(m.tree, root), !diverged, nil
}

// applyOpened opens layer i through openLayer, hands its stream to apply, and
// closes it, joining a close failure with the apply failure so neither is
// dropped. At most one layer is open at a time.
func applyOpened(openLayer func(int) (io.ReadCloser, error), i int, apply func(io.Reader) error) error {
	layer, err := openLayer(i)
	if err != nil {
		return fmt.Errorf("rootfs: open layer %d: %w", i, err)
	}
	if layer == nil {
		return fmt.Errorf("rootfs: layer[%d] is nil", i)
	}
	applyErr := apply(layer)
	closeErr := layer.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("rootfs: layer %d: close: %w", i, closeErr)
	}
	if applyErr != nil || closeErr != nil {
		return errors.Join(applyErr, closeErr)
	}
	return nil
}

// Layer is one scanned layer: its entries in stream order, each normalized and
// with every regular file's content captured, so merging it into a tree needs
// neither the stream nor the sink. Whiteout and opaque markers stay entries and
// take effect at merge time, since their effect depends on the layers below.
type Layer struct {
	idx     int
	records []record
}

// entryKind classifies a scanned tar entry by how merge applies it; the
// type of a node entry is in its mode.
type entryKind uint8

const (
	kindNode     entryKind = iota // installs the record's node
	kindHardlink                  // another name of its target's inode
	kindWhiteout                  // .wh.<name>: removes target and everything beneath it
	kindOpaque                    // .wh..wh..opq: removes the lower-layer content of its directory
)

// record is one normalized entry of a scanned layer. n is the node the entry
// installs, complete but for a stored payload's fs-verity digest while that is
// still pending; a hardlink or marker carries no node — a hardlink's node
// points at its target's and is made at merge time, once the target is known.
// target is a hardlink's normalized target name or the path a whiteout removes.
type record struct {
	kind   entryKind
	name   string
	target string
	n      *node
}

// scanLayer walks one decompressed layer tar and returns it as a Layer of
// normalized records in stream order — a regular file's content captured
// (stored through sink, or read inline) and every other entry's metadata
// converted — after verifying the whole decompressed stream hashes to its
// diff_id at end of stream. The stream is teed into the hash as the tar reader
// consumes it, so content is captured and hashed toward the diff_id in one
// pass. Whatever can be checked without the merged tree — names, link targets,
// entry types, the sink a stored file needs — is checked here, before the next
// entry is read.
func scanLayer(ctx context.Context, sink ObjectSink, layer io.Reader, diffID digest.Digest, idx int) (*Layer, error) {
	l := &Layer{idx: idx}
	h := sha256.New()
	teed := io.TeeReader(&ctxReader{ctx: ctx, r: layer}, h)
	tr := tar.NewReader(teed)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rootfs: layer %d: read tar: %w", idx, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name, err := normalizeName(hdr.Name)
		if err != nil {
			return nil, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		if name == "" {
			continue
		}
		rec, err := scanHeader(ctx, sink, tr, hdr, name, idx)
		if err != nil {
			return nil, fmt.Errorf("rootfs: layer %d: %w", idx, err)
		}
		l.records = append(l.records, rec)
	}
	if _, err := io.Copy(io.Discard, teed); err != nil {
		return nil, fmt.Errorf("rootfs: layer %d: drain: %w", idx, err)
	}
	if got := digest.NewDigest(digest.SHA256, h); got != diffID {
		return nil, fmt.Errorf("rootfs: layer %d diff_id mismatch: expected %s, got %s", idx, diffID, got)
	}
	return l, nil
}

// scanHeader converts one tar entry into a record, capturing a regular file's
// content: a file of at most composefs.InlineContentMax bytes has its bytes
// read inline, a larger one streams from tr into the object sink (recording
// its content digest and pending fs-verity digest).
func scanHeader(ctx context.Context, sink ObjectSink, tr *tar.Reader, hdr *tar.Header, name string, idx int) (record, error) {
	return recordFromHeader(hdr, name, idx, func(n *node) error {
		if hdr.Size <= composefs.InlineContentMax {
			// A small file is inlined into the EROFS metadata: read its bytes (the
			// tar reader caps the read at the entry size) and skip the object store.
			content, err := io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("read inline content for %s: %w", name, err)
			}
			n.content = content
			n.size = int64(len(content))
			return nil
		}
		if sink == nil {
			return errors.New("regular file requires object sink")
		}
		d, sealed, err := sink.WriteObject(ctx, tr)
		if err != nil {
			return fmt.Errorf("write object for %s: %w", name, err)
		}
		n.digest = d
		n.sealed = sealed
		n.size = hdr.Size
		return nil
	})
}

// recordFromHeader converts one layer entry, described by its tar header and
// normalized name, into a record: a whiteout or opaque marker by its base
// name, otherwise by type — a regular file's node has its payload filled in by
// payload, a hardlink keeps its normalized target, and the remaining types
// become metadata-only nodes. Every node carries the entry's modification
// time.
func recordFromHeader(hdr *tar.Header, name string, idx int, payload func(*node) error) (record, error) {
	dir, base := splitPath(name)
	switch {
	case base == ".wh..wh..opq":
		return record{kind: kindOpaque, name: name}, nil
	case strings.HasPrefix(base, ".wh."):
		target := strings.TrimPrefix(base, ".wh.")
		if target == "" {
			return record{}, fmt.Errorf("empty whiteout target %q", hdr.Name)
		}
		return record{kind: kindWhiteout, name: name, target: path.Join(dir, target)}, nil
	}

	rec := record{kind: kindNode, name: name}
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00': // '\x00' is the legacy regular-file flag (formerly tar.TypeRegA)
		rec.n = nodeFromHeader(hdr, 0, idx)
		if err := payload(rec.n); err != nil {
			return record{}, err
		}
	case tar.TypeLink:
		target, err := normalizeName(hdr.Linkname)
		if err != nil {
			return record{}, fmt.Errorf("hardlink %s target: %w", name, err)
		}
		rec.kind = kindHardlink
		rec.target = target
	case tar.TypeSymlink:
		if hdr.Linkname == "" {
			return record{}, fmt.Errorf("symlink %s has empty target", name)
		}
		rec.n = nodeFromHeader(hdr, os.ModeSymlink, idx)
		rec.n.linkname = hdr.Linkname
	case tar.TypeDir:
		rec.n = nodeFromHeader(hdr, os.ModeDir, idx)
	case tar.TypeChar:
		rec.n = deviceNode(hdr, os.ModeDevice|os.ModeCharDevice, idx)
	case tar.TypeBlock:
		rec.n = deviceNode(hdr, os.ModeDevice, idx)
	case tar.TypeFifo:
		rec.n = deviceNode(hdr, os.ModeNamedPipe, idx)
	default:
		return record{}, fmt.Errorf("unsupported tar entry type %q for %s", hdr.Typeflag, name)
	}
	return rec, nil
}

// resolve fills in the fs-verity digest of every stored payload once its
// object is sealed, waiting as needed.
func (l *Layer) resolve() error {
	for _, rec := range l.records {
		if rec.n == nil || rec.n.sealed == nil {
			continue
		}
		if err := resolveNode(rec.name, rec.n); err != nil {
			return err
		}
	}
	return nil
}

// mergeRecord merges one scanned entry of layer idx into tree: a marker
// removes what it names, a hardlink becomes another name of its target's
// inode, and any other entry installs its node. The bool reports whether the
// entry diverged from the upstream-reproducible construction: it (or a marker)
// reached a name whose immediate parent was absent at that moment — upstream
// errors there while ocicfs synthesises the parent later — or an opaque marker
// cleared same-layer content.
func mergeRecord(tree map[string]*node, rec record, idx int) (bool, error) {
	dir, _ := splitPath(rec.name)
	// Upstream resolves the parent chain before inserting a name or applying a
	// marker and errors if any component is missing or not a directory; ocicfs
	// instead repairs the tree after merging, so a name whose immediate parent
	// is absent — or present as a non-directory — right now is a divergence.
	// The immediate-parent check accumulates the whole chain by induction: a
	// parent inserted while absent flagged its own name.
	diverged := false
	if dir != "" {
		if p, ok := tree[dir]; !ok || p.mode&os.ModeDir == 0 {
			diverged = true
		}
	}
	switch rec.kind {
	case kindOpaque:
		if removeOpaque(tree, dir, idx) {
			diverged = true
		}
		return diverged, nil
	case kindWhiteout:
		removeSubtree(tree, rec.target)
		return diverged, nil
	}

	// A non-directory entry replaces whatever occupied name. If that was a
	// directory, its children lose their parent, so drop the whole subtree
	// before inserting the replacement (last-writer-wins, matching overlay
	// merge semantics). A directory entry instead merges over any existing
	// node and keeps children.
	if rec.kind == kindHardlink || rec.n.mode&os.ModeDir == 0 {
		if existing, ok := tree[rec.name]; ok && existing.mode&os.ModeDir != 0 {
			removeSubtree(tree, rec.name)
		}
	}
	if rec.kind == kindHardlink {
		if err := linkHardlink(tree, rec.name, rec.target, idx); err != nil {
			return diverged, fmt.Errorf("rootfs: layer %d: hardlink %s -> %s: %w", idx, rec.name, rec.target, err)
		}
		return diverged, nil
	}
	tree[rec.name] = rec.n
	return diverged, nil
}

// nodeFromHeader builds a metadata-only node of the given type from a tar header.
func nodeFromHeader(hdr *tar.Header, typ os.FileMode, idx int) *node {
	sec, nsec := mtimeFromHeader(hdr)
	return &node{
		mode:      modeFromHeader(hdr, typ),
		uid:       hdr.Uid,
		gid:       hdr.Gid,
		xattrs:    xattrsFromHeader(hdr),
		mtimeSec:  sec,
		mtimeNsec: nsec,
		layer:     idx,
	}
}

// deviceNode builds a metadata-only node for a character, block, or fifo special file.
func deviceNode(hdr *tar.Header, typ os.FileMode, idx int) *node {
	n := nodeFromHeader(hdr, typ, idx)
	n.devmajor = hdr.Devmajor
	n.devminor = hdr.Devminor
	return n
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

// resolveNode waits for the object behind n, named name, to be sealed and
// records its fs-verity digest.
func resolveNode(name string, n *node) error {
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
// drops such entries, while ocicfs keeps them per OCI layer semantics — a
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

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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
