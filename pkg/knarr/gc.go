// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocilayout"
)

// gcBetweenPhases, when set, runs after the inventory and before gc converts
// the store lock to exclusive. It is nil outside tests, which use it to
// interleave a verb with a collection.
var gcBetweenPhases func()

// GC deletes every store object that is neither wanted nor in use.
func (e *engine) GC(ctx context.Context, opts ...GCOption) (*GCReport, error) {
	report, err := e.gc(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gc: %w", err)
	}
	return report, nil
}

func (e *engine) gc(ctx context.Context, opts ...GCOption) (*GCReport, error) {
	var cfg GCOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	// The lock is taken exclusive first, so a verb in flight fails the
	// collection before any work, then converted to shared for the
	// inventory, which fs-verity-measures every object and dominates the
	// run time: pulls, holds, and mounts proceed alongside it. The
	// conversion to exclusive that follows fails if a verb started during
	// the inventory and is still running; every conversion is non-blocking
	// and a failed one leaves no lock, so the release only closes the file.
	lock, err := e.openStoreLock(unix.LOCK_EX | unix.LOCK_NB)
	if err != nil {
		return nil, err
	}
	unlock := releaseStoreLock(lock)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := flockStore(lock, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return nil, err
	}
	start := time.Now()
	inventory, err := e.inventoryObjects(ctx)
	if err != nil {
		return nil, err
	}
	report := &GCReport{InventoryDuration: time.Since(start)}

	if gcBetweenPhases != nil {
		gcBetweenPhases()
	}

	// Decisions and deletions run under the exclusive lock from fresh
	// listings, so a reference or hold recorded during the inventory marks
	// what it wants before anything is judged.
	if err := flockStore(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	start = time.Now()
	marks, err := e.markWanted(ctx)
	if err != nil {
		return nil, err
	}
	if err := e.sweepRuntimeRefs(ctx, cfg.DryRun, report); err != nil {
		return nil, err
	}
	if err := e.sweepBindings(ctx, marks, cfg.DryRun, report); err != nil {
		return nil, err
	}
	if err := e.sweepSavedLayers(ctx, marks, cfg.DryRun); err != nil {
		return nil, err
	}

	var candidates []gcObject
	for _, o := range inventory {
		if _, ok := marks.sha[o.digest]; ok {
			continue
		}
		if _, ok := marks.objects[o.viewName]; ok {
			continue
		}
		candidates = append(candidates, o)
	}
	report.Candidates = len(candidates)

	// The in-use probe runs over every candidate before the first deletion:
	// an in-use image's redirects join the mark set — the transitive
	// protection — so no data object of a live mount can already be gone by
	// the time its image is visited.
	candidates, err = e.probeInUse(ctx, marks, inventory, candidates, report)
	if err != nil {
		return nil, err
	}
	report.MarkSweepDuration = time.Since(start)

	// Delete what was judged unwanted and idle and the transitive marks did
	// not claim since.
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, ok := marks.objects[c.viewName]; ok {
			continue
		}
		if !cfg.DryRun {
			err := e.obj.Remove(c.digest)
			if errors.Is(err, objectstore.ErrInUse) {
				report.InUseSkipped++
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		report.ObjectsRemoved++
		report.BytesFreed += c.size
		report.Removed = append(report.Removed, c.digest.String())
		slog.Debug("knarr: gc removed object", "digest", c.digest, "size", c.size, "dryRun", cfg.DryRun)
	}

	if !cfg.DryRun {
		// A view orphan whose name the mark set claims is the last copy of
		// wanted content — the CAS name was lost to external damage — and the
		// sweep keeps it, since the view name is the copy mounts serve.
		orphans, err := e.obj.SweepViewOrphans(ctx, func(name string) bool {
			_, ok := marks.objects[name]
			return ok
		})
		if err != nil {
			return nil, err
		}
		report.ObjectsRemoved += orphans
		if err := clearDir(e.obj.TempDir()); err != nil {
			return nil, err
		}
		// Deletion leaves empty shard directories across the stores' fan-out
		// trees; prune them while the exclusive lock still excludes any
		// writer that could be re-creating one. The space is cosmetic, so a
		// failure warns rather than aborts the collection.
		for _, prune := range []func() error{
			e.obj.PruneEmptyDirs, e.img.PruneEmptyDirs, e.refs.PruneEmptyDirs, e.holds.PruneEmptyDirs,
		} {
			if err := prune(); err != nil {
				slog.Warn("knarr: gc failed to prune empty directories", "err", err)
			}
		}
		// One syncfs makes the reclaimed space durable before the report
		// claims it; no per-object fsync.
		if err := e.obj.Sync(); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// gcMarks accumulates everything the marking passes decide to keep. The
// sweep deletes whatever is in none of the first four sets, so an entry
// here is a promise of survival. Content is marked in the two namespaces
// objects are named by — sha256 content digests, and the view-relative
// fs-verity names that are the only way an EROFS refers to per-file
// objects — hence two object sets. The last two sets mark nothing; they
// only dedup work across roots that name the same image.
type gcMarks struct {
	// sha keeps objects by content digest: the metadata chain (manifest,
	// config) and the composefs EROFS images, plus held loose objects.
	sha map[digest.Digest]struct{}

	// objects keeps per-file data objects by their view-relative fs-verity
	// name ("ab/cdef..."), the name the EROFS redirect xattrs carry.
	objects map[string]struct{}

	// wanted keeps binding directories, keyed by the image identity digest
	// (the platform image manifest); sweepBindings deletes the rest.
	wanted map[digest.Digest]struct{}

	// layers keeps saved layers, keyed by the digest of the compressed layer
	// blob a wanted image's manifest lists; sweepSavedLayers deletes the
	// rest. It marks no object: a layer blob is never stored, and a saved
	// layer keeps none of the files it lists alive.
	layers map[digest.Digest]struct{}

	// verified dedups the chain-resolution and identity check, keyed by
	// "root|identity|platform". Every distinct root must be checked — a
	// broken alias would otherwise hide behind a healthy one.
	verified map[string]struct{}

	// marked dedups the expensive half — binding fs-verity verification and
	// the EROFS redirect walk — keyed by "identity|platform", so many refs
	// and holds naming one image walk it once.
	marked map[string]struct{}
}

// markWanted builds the wanted set from the store's roots: every reference
// record and every held digest. Both listings are strict, and any wanted
// image that cannot be fully enumerated — an unresolvable chain, a missing
// binding, an unreadable EROFS — aborts the collection: sweeping anyway could
// delete live data an offline device cannot re-fetch, so keeping garbage one
// cycle longer is always the safe side.
func (e *engine) markWanted(ctx context.Context) (*gcMarks, error) {
	m := &gcMarks{
		sha:      map[digest.Digest]struct{}{},
		objects:  map[string]struct{}{},
		wanted:   map[digest.Digest]struct{}{},
		layers:   map[digest.Digest]struct{}{},
		verified: map[string]struct{}{},
		marked:   map[string]struct{}{},
	}

	records, err := e.refs.ListStrict(ctx)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A runtime reference names an image on a holder's behalf and pins
		// nothing: the hold marks the image, and the sweep drops the name
		// once the holder is gone.
		if rec.Holder != "" {
			continue
		}
		m.sha[rec.Resolved.Digest] = struct{}{}
		m.sha[rec.Identity.Digest] = struct{}{}
		m.wanted[rec.Identity.Digest] = struct{}{}
		platforms, err := parseRecordPlatforms(rec.Platforms)
		if err != nil {
			return nil, fmt.Errorf("ref %s: %w", rec.Ref, err)
		}
		onDisk, err := e.img.Platforms(ctx, rec.Identity.Digest)
		if err != nil {
			return nil, fmt.Errorf("ref %s: %w", rec.Ref, err)
		}
		for _, p := range append(platforms, onDisk...) {
			if err := e.markImage(ctx, m, rec.Resolved.Digest, rec.Identity.Digest, p, "ref "+rec.Ref); err != nil {
				return nil, err
			}
		}
	}

	held, err := e.holds.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, root := range held {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.sha[root] = struct{}{}
		kind, err := e.heldRootKind(root)
		if err != nil {
			return nil, err
		}
		if kind == heldLoose {
			// A held loose object: the digest alone is the mark. It joins no
			// binding-protecting set — a binding directory keyed by the same
			// digest is unrelated and sweeps normally.
			continue
		}
		m.wanted[root] = struct{}{}
		onDisk, err := e.img.Platforms(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("held %s: %w", root, err)
		}
		if len(onDisk) == 0 {
			if kind == heldIndex {
				// Bindings are keyed by the platform-manifest identity, never
				// by an index, so a pull can never satisfy this hold; only
				// releasing it can.
				return nil, fmt.Errorf("held digest %s is an image index, which no binding can ever be keyed by; `knarr release` it and hold the image identity instead", root)
			}
			return nil, fmt.Errorf("held image %s has no binding, so its objects cannot be enumerated; run `knarr pull` to restore it or `knarr release` to stop wanting it", root)
		}
		for _, p := range onDisk {
			if err := e.markImage(ctx, m, root, root, p, "hold"); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

// markImage marks one wanted image: the hash-verified metadata chain root
// anchors for platform p, the saved layers of the layer blobs its manifest
// lists, the binding's composefs object, and every per-file object that
// EROFS redirects to. The chain must land on wantIdentity — the digest the
// record or hold protects — or the root is broken, not merely a different
// image.
func (e *engine) markImage(ctx context.Context, m *gcMarks, root, wantIdentity digest.Digest, p ocispec.Platform, origin string) error {
	// Every distinct root is resolved and must land on the recorded identity
	// — a broken alias must abort, not slide past because another record
	// already vouched for the identity.
	verifyKey := root.String() + "|" + wantIdentity.String() + "|" + ocilayout.PlatformString(&p)
	if _, ok := m.verified[verifyKey]; ok {
		return nil
	}
	m.verified[verifyKey] = struct{}{}

	res, err := e.resolveStoredImage(ctx, root, p)
	if err != nil {
		return fmt.Errorf("wanted image %s (%s): %w", wantIdentity, origin, err)
	}
	if res.ManifestDesc.Digest != wantIdentity {
		return fmt.Errorf("wanted image %s (%s): root %s resolves to manifest %s for platform %s; repair the record with `knarr pull` or `knarr remove`", wantIdentity, origin, root, res.ManifestDesc.Digest, ocilayout.PlatformString(&p))
	}
	m.sha[res.ManifestDesc.Digest] = struct{}{}
	m.sha[res.Manifest.Config.Digest] = struct{}{}
	for _, layer := range res.Manifest.Layers {
		m.layers[layer.Digest] = struct{}{}
	}

	// The expensive half — binding verification and the EROFS redirect walk —
	// is per image, not per record: aliases naming one identity walk it once.
	// The key uses the resolved platform, the spelling bindings are keyed by,
	// so equivalent inputs (a recorded default variant vs its canonical form)
	// land on one key.
	markKey := wantIdentity.String() + "|" + ocilayout.PlatformString(&res.Platform)
	if _, ok := m.marked[markKey]; ok {
		return nil
	}
	m.marked[markKey] = struct{}{}

	b, err := e.img.Get(wantIdentity, res.Platform)
	if errors.Is(err, imagestore.ErrNotFound) {
		return fmt.Errorf("wanted image %s (%s) has no binding for platform %s, so its objects cannot be enumerated; run `knarr pull` to restore it, or `knarr remove`/`knarr release` to stop wanting it", wantIdentity, origin, ocilayout.PlatformString(&res.Platform))
	}
	if err != nil {
		return fmt.Errorf("wanted image %s (%s): %w", wantIdentity, origin, err)
	}

	// The binding is an untrusted, regenerable index, and here its selection
	// decides what gc preserves. Enforce the same fs-verity expectation a
	// mount does — the sealing annotation of the verified identity manifest,
	// or the binding's own recorded digest for an unsealed image — before
	// trusting it as deletion authority.
	expected, _, err := expectedFsverity(res.Manifest.Annotations, b.FsverityDigest)
	if err != nil {
		return fmt.Errorf("wanted image %s (%s): %w", wantIdentity, origin, err)
	}
	measured, err := e.obj.VerityDigest(b.Descriptor.Digest)
	if err != nil {
		return fmt.Errorf("wanted image %s (%s): verify composefs image %s: %w; run `knarr pull` to restore it, or `knarr remove`/`knarr release` to stop wanting it", wantIdentity, origin, b.Descriptor.Digest, err)
	}
	if measured != expected {
		return fmt.Errorf("wanted image %s (%s): composefs image %s measures fs-verity %s, expected %s; the binding does not select this image's own composefs object — run `knarr remove` then `knarr pull` to repair it", wantIdentity, origin, b.Descriptor.Digest, measured, expected)
	}
	m.sha[b.Descriptor.Digest] = struct{}{}

	imagePath, err := e.obj.ObjectPath(b.Descriptor.Digest)
	if err != nil {
		return err
	}
	names, err := composefs.ReferencedObjects(ctx, imagePath)
	if err != nil {
		return fmt.Errorf("wanted image %s (%s): %w; run `knarr pull` to restore it, or `knarr remove`/`knarr release` to stop wanting it", wantIdentity, origin, err)
	}
	for _, name := range names {
		m.objects[name] = struct{}{}
	}
	return nil
}

// heldKind classifies what a held digest's stored blob is, decisively.
type heldKind int

const (
	heldLoose heldKind = iota
	heldManifest
	heldIndex
)

// heldRootKind classifies a held digest's stored blob: it is an image root
// only when it carries OCI structural markers — a known index or manifest
// media type, a manifests array, or a config digest with layers. Anything
// else, including arbitrary JSON that would happily unmarshal into either
// OCI struct, is a held loose object; routing one into chain resolution
// would fail and abort every collection until the hold is released. A held
// digest whose blob is absent is a broken wanted root and aborts: hold
// records holders only for present content, so the content was deleted
// behind the hold's back.
func (e *engine) heldRootKind(root digest.Digest) (heldKind, error) {
	f, err := e.obj.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		return heldLoose, fmt.Errorf("held object %s is missing from the store; run `knarr pull` to restore it or `knarr release` to stop wanting it", root)
	}
	if err != nil {
		return heldLoose, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("knarr: failed to close held object", "digest", root, "err", err)
		}
	}()
	// Larger than any metadata blob a pull stores: cannot be an image root.
	const maxProbe = 32 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxProbe+1))
	if err != nil {
		return heldLoose, fmt.Errorf("read held object %s: %w", root, err)
	}
	if len(data) > maxProbe {
		return heldLoose, nil
	}
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
		Config    struct {
			Digest digest.Digest `json:"digest"`
		} `json:"config"`
		Layers []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return heldLoose, nil
	}
	switch probe.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		return heldIndex, nil
	case ocispec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		return heldManifest, nil
	}
	if probe.Manifests != nil {
		return heldIndex, nil
	}
	if probe.Config.Digest != "" && probe.Layers != nil {
		return heldManifest, nil
	}
	return heldLoose, nil
}

// sweepRuntimeRefs deletes every runtime reference whose holder no longer
// stands on the identity it names: the name lived on the hold's behalf, and
// the image it named is unwanted, or wanted by someone else, from here on.
func (e *engine) sweepRuntimeRefs(ctx context.Context, dryRun bool, report *GCReport) error {
	records, err := e.refs.ListStrict(ctx)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rec.Holder == "" {
			continue
		}
		holders, err := e.holds.Holders(rec.Identity.Digest)
		if err != nil {
			return err
		}
		if slices.Contains(holders, rec.Holder) {
			continue
		}
		if !dryRun {
			if err := e.refs.Delete(rec.Ref); err != nil {
				return err
			}
		}
		report.RefsRemoved++
		slog.Debug("knarr: gc removed runtime reference", "ref", rec.Ref, "holder", rec.Holder, "dryRun", dryRun)
	}
	return nil
}

// sweepBindings deletes bindings whose identity no reference record and no
// hold reaches — the orphans a crashed remove leaves behind.
func (e *engine) sweepBindings(ctx context.Context, m *gcMarks, dryRun bool, report *GCReport) error {
	return e.img.Walk(ctx, func(root digest.Digest, p ocispec.Platform) error {
		if _, ok := m.wanted[root]; ok {
			return nil
		}
		if !dryRun {
			if err := e.img.Delete(root, p); err != nil {
				return err
			}
		}
		report.BindingsRemoved++
		slog.Debug("knarr: gc removed orphan binding", "identity", root, "platform", ocilayout.PlatformString(&p), "dryRun", dryRun)
		return nil
	})
}

// sweepSavedLayers deletes every saved layer whose layer blob no wanted
// image's manifest lists: only a pull of an image naming that blob could use
// it, and one that does reads the layer again.
func (e *engine) sweepSavedLayers(ctx context.Context, m *gcMarks, dryRun bool) error {
	return e.img.WalkLayers(ctx, func(blob digest.Digest) error {
		if _, ok := m.layers[blob]; ok {
			return nil
		}
		if !dryRun {
			if err := e.img.DeleteLayer(blob); err != nil {
				return err
			}
		}
		slog.Debug("knarr: gc removed saved layer", "layer", blob, "dryRun", dryRun)
		return nil
	})
}

// gcObject is one stored object as the inventory saw it: its content digest,
// size, and the view-relative fs-verity name EROFS redirects carry.
type gcObject struct {
	digest   digest.Digest
	size     int64
	viewName string
}

// inventoryObjects walks every object and measures it (a cheap read of the
// sealed fs-verity root hash). An object that cannot be measured is skipped
// with a warning, never deleted: wanted per-file objects are marked only by
// fs-verity name, so an unmeasurable object cannot be proven unwanted.
func (e *engine) inventoryObjects(ctx context.Context) ([]gcObject, error) {
	var inventory []gcObject
	err := e.obj.Walk(ctx, func(d digest.Digest, size int64) error {
		fsv, err := e.obj.VerityDigest(d)
		if err != nil {
			slog.Warn("knarr: gc skipping unmeasurable object", "digest", d, "err", err)
			return nil
		}
		inventory = append(inventory, gcObject{digest: d, size: size, viewName: viewRelativeName(fsv)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inventory, nil
}

// probeInUse drops every in-use candidate, counting it in the report, and
// returns the rest. It also probes the objects written since the inventory,
// which are never candidates this cycle, so that an image mounted from them
// marks the inventoried objects it redirects to.
func (e *engine) probeInUse(ctx context.Context, m *gcMarks, inventory, candidates []gcObject, report *GCReport) ([]gcObject, error) {
	var idle []gcObject
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inUse, err := e.markInUse(ctx, m, c.digest)
		if err != nil {
			return nil, err
		}
		if inUse {
			report.InUseSkipped++
			continue
		}
		idle = append(idle, c)
	}
	inventoried := make(map[digest.Digest]struct{}, len(inventory))
	for _, o := range inventory {
		inventoried[o.digest] = struct{}{}
	}
	err := e.obj.Walk(ctx, func(d digest.Digest, _ int64) error {
		if _, ok := inventoried[d]; ok {
			return nil
		}
		_, err := e.markInUse(ctx, m, d)
		return err
	})
	if err != nil {
		return nil, err
	}
	return idle, nil
}

// markInUse reports whether a mounter holds object d and, when one does,
// adds the objects it redirects to to the mark set.
func (e *engine) markInUse(ctx context.Context, m *gcMarks, d digest.Digest) (bool, error) {
	inUse, err := e.obj.InUse(d)
	if err != nil || !inUse {
		return false, err
	}
	path, err := e.obj.ObjectPath(d)
	if err != nil {
		return false, err
	}
	names, err := composefs.ReferencedObjects(ctx, path)
	if err != nil {
		// Only composefs images carry mount locks, but the probe is
		// advisory: an in-use object that does not read as one is simply
		// kept, contributing nothing transitively.
		slog.Debug("knarr: gc kept in-use object that is not a composefs image", "digest", d, "err", err)
		return true, nil
	}
	for _, n := range names {
		m.objects[n] = struct{}{}
	}
	return true, nil
}

// viewRelativeName maps a measured fs-verity digest to the sharded
// view-relative name the EROFS redirects carry.
func viewRelativeName(fsv string) string {
	hex := strings.TrimPrefix(fsv, "sha256:")
	if len(hex) < 2 {
		return hex
	}
	return hex[:2] + "/" + hex[2:]
}

// clearDir removes the contents of dir, keeping dir itself. gc holds the
// exclusive store lock, so nothing under the staging directory is live.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("clear %s: %w", dir, err)
		}
	}
	return nil
}
