// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ocicfs is the engine API for the ocicfs content delivery and mount
// tool. It exposes the primitive operations exposed to the command-line front
// end and to embedders. The engine owns a local content-addressed store; it
// holds no version-management, activation, or update-orchestration policy.
//
// The current surface covers the basic primitives — pull, mount, unmount,
// inspect, list, hold, release, remove, gc. Verification runs inside Pull and
// Mount. Publish is declared but returns ErrNotImplemented; the remaining
// primitives (verify, status) are added as they are implemented.
package ocicfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/holdstore"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/refstore"
)

// Image describes an image present in the local store.
type Image struct {
	// Ref is the local reference the image is stored under.
	Ref string
	// Digest is the image's identity: the digest of the platform image
	// manifest the store keys it by, e.g. "sha256:…". A reference that pins or
	// resolves to an image index is descended to that manifest, so this is the
	// digest to pin, hold, and look the image up by.
	Digest string
	// Size is the size of the image's composefs image object, in bytes; file
	// content shared in the object store is not counted.
	Size int64
	// Holder is the hold a runtime reference names the image for: the
	// reference then pins nothing, and goes when that holder is released.
	// Empty for an agent's reference, which keeps the image on its own, and
	// for an image a hold alone keeps under no reference.
	Holder string
}

// ImageInfo is the identity of a locally stored image: its OCI coordinates and
// the composefs image built for it.
type ImageInfo struct {
	// Root is the image's identity: the digest of the platform image manifest
	// the store keys it by, whether the reference named that manifest or the
	// index offering it.
	Root digest.Digest
	// Platform is the resolved platform, as declared by the image config (it
	// may carry a variant the host request omitted, e.g. arm64/v8).
	Platform ocispec.Platform
	// DiffIDs are the config's ordered rootfs.diff_ids (sha256, one per layer).
	DiffIDs []digest.Digest
	// Composefs is the descriptor of the composefs image blob recorded for
	// {Root, Platform}; its digest names the blob in the object store.
	Composefs ocispec.Descriptor
	// FsverityDigest is the locally measured fs-verity digest recorded for
	// the composefs blob at build time; a mount enforces it only when the
	// identity manifest carries no sealing annotation.
	FsverityDigest string
	// RootfsSize is the apparent size of the merged rootfs recorded when the
	// image was built: its regular-file content bytes, each hardlink group
	// counted once. It is not the on-disk cost (content is shared in the
	// object store). 0 means the binding predates the field; it is recorded
	// when the image is next actually built (a pull that verifies the image
	// as already present does not rewrite the binding).
	RootfsSize int64
	// Layers describes each filesystem layer of the resolved manifest, in
	// manifest order.
	Layers []LayerInfo
}

// LayerInfo describes one filesystem layer of a resolved manifest; a layer's
// index is its position in ImageInfo.Layers.
type LayerInfo struct {
	// MediaType is the layer descriptor's media type from the manifest.
	MediaType string
	// Digest names the layer blob as transferred (manifest layers[i].digest).
	Digest digest.Digest
	// Size is the layer blob's byte size declared by the manifest descriptor:
	// the archive as transferred, compressed for +gzip/+zstd media types and
	// the bare tar size for uncompressed layers.
	Size int64
	// DiffID is the digest of the layer's uncompressed tar stream
	// (config rootfs.diff_ids[i]).
	DiffID digest.Digest
	// ChainID is the cumulative identity of the layer stack up to and
	// including this layer, derived from the ordered diff_ids.
	ChainID digest.Digest
	// CreatedBy is the config history command correlated to this layer. It is
	// empty when the config records no history or when its non-empty entries
	// do not match the layer count, in which case no correlation is guessed.
	CreatedBy string
}

// BlobSource opens the raw blob bytes for a descriptor. An embedder supplies
// one to pull an image out of content it already holds, for example a container
// runtime's content store. Open may be called concurrently, and a build calls
// it for layers ahead of the ones the caller has asked for: an Open that
// fails for such a layer, however the source reports a blob it does not hold
// yet, is retried until the caller asks for that layer. Open may do I/O, but
// it must report a blob it does not currently hold rather than wait for one to
// arrive, and it must return promptly once ctx is cancelled.
type BlobSource interface {
	Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error)
}

// Build is an image pull whose layers the caller feeds one at a time, for an
// embedder that receives an image's layers incrementally — a container runtime
// unpacking a pull — and wants each one materialized as it lands rather than
// all of them at the end. From the first Ingest or Finish on, a build scans
// layers ahead of the caller's calls in the background, under the engine's
// worker cap; it keeps the values of the
// NewBuild context but not its cancellation, so a build may span request
// handlers, and Finish or Close stops whatever it started.
//
// Calls are serialized: a call that arrives while another is in progress
// waits for it, except Close, which interrupts a blocked Ingest or Finish. A
// call on a build that has finished, been closed, or failed returns an error
// — after a failure, the same error every time. The store is held against gc
// from NewBuild until Finish or Close returns.
type Build interface {
	// Ingest blocks until every layer up to and including index i is
	// materialized, reading them from the pull's blob source. Layers are
	// applied in manifest order: a jump forward applies every layer in
	// between, and an index at or below one already ingested returns at
	// once. The failure of layer i — or of any layer scanned ahead of it —
	// is reported here. A cancelled ctx returns its error and leaves the
	// build running, so the call may be repeated. On a build whose image the
	// store already held at NewBuild it does nothing at all. It is an error
	// after Finish or Close.
	Ingest(ctx context.Context, i int) error
	// Finish ingests any remaining layers, builds and records the image, and
	// returns it; for an image the store already held it records the
	// reference alone, as Pull does. It drains every scan before returning,
	// whether or not it succeeded, and the build is spent afterwards.
	Finish(ctx context.Context) (*Image, error)
	// Close abandons an unfinished build: it cancels every scan and waits
	// for them, interrupting an Ingest or Finish blocked meanwhile, then
	// releases the store; objects already sealed stay in the store as
	// unreferenced content for gc. Idempotent, and a no-op after Finish.
	Close()
}

// PullOptions is the resolved configuration a Pull acts on. Callers pass the
// With* options to Pull, which apply to a zero PullOptions. Exactly one
// source feeds a pull: FromDir, BlobSource, or — when neither is set — the
// engine's configured RegistrySource; setting both options is an error.
type PullOptions struct {
	// FromDir sources content from an OCI image layout directory instead of a
	// registry (air-gapped sideload). Empty leaves source selection to the
	// other options or the engine's RegistrySource.
	FromDir string
	// BlobSource sources every blob from the caller instead of a registry or a
	// layout directory. Nil leaves source selection to FromDir or the engine's
	// RegistrySource.
	BlobSource BlobSource
	// ResolvedRoot is the root digest the pull anchors on, for a caller that
	// has already resolved the reference. Empty means no caller-supplied
	// resolution, like the zero value of every other field here: a pinned
	// reference then carries its digest, and a configured RegistrySource
	// resolves a tag itself.
	ResolvedRoot digest.Digest
	// Hold names a holder the pull records on the image's identity digest
	// instead of a reference record for ref. Empty means the reference is
	// recorded, as every plain pull does.
	Hold string
	// Delta fetches, of every zstd:chunked layer, only its table of contents
	// and the files the store lacks, as byte ranges. It needs a source that
	// serves ranges: the registry source and a layout directory do, a
	// caller's BlobSource only when its type also implements OpenRange. It
	// is available through Pull only.
	Delta bool
}

// PullOption opts a Pull into a particular behaviour.
type PullOption func(*PullOptions)

// WithFromDir sources content from an OCI image layout directory instead of
// the engine's RegistrySource (air-gapped sideload).
func WithFromDir(dir string) PullOption {
	return func(o *PullOptions) { o.FromDir = dir }
}

// WithBlobSource reads every blob of the pull — root, manifest, config, and
// layers — from src instead of the engine's RegistrySource. The reference must
// pin the root digest unless WithResolvedRoot supplies it. It cannot be combined
// with WithFromDir.
func WithBlobSource(src BlobSource) PullOption {
	return func(o *PullOptions) { o.BlobSource = src }
}

// WithResolvedRoot anchors the pull on a root digest the caller resolved
// itself, so a reference that pins no digest — a tag — can still be pulled.
// The reference is then recorded only as an alias for that digest, exactly as
// a RegistrySource tag resolution records it, so a later local lookup of the
// reference finds the image. The digest need not be the one the reference
// pins: a caller may pull a platform manifest out of an index-pinned
// reference, and doing so stores the same image, under the same identity, as
// pulling the index would. It requires WithBlobSource.
func WithResolvedRoot(root digest.Digest) PullOption {
	return func(o *PullOptions) { o.ResolvedRoot = root }
}

// WithHold makes the pull keep the image by a hold under holder on its
// identity digest, taken once the image is in the store and before the pull
// releases it to gc, so the image is never collectable in between; ref is
// then recorded as a runtime reference, a name that pins nothing and that gc
// drops once the holder is released. The image is kept for exactly as long
// as the holder stands. A runtime reference never displaces a reference an
// agent's plain pull recorded for the same name — the image is then held but
// not named — while a plain pull of the name replaces a runtime reference and
// keeps the image on its own. A holder name the hold store rejects fails the
// pull before anything is fetched. Release drops the hold; gc reclaims the
// image once nothing holds or names it.
func WithHold(holder string) PullOption {
	return func(o *PullOptions) { o.Hold = holder }
}

// WithDelta makes the pull fetch, of every zstd:chunked layer, only its
// table of contents and the files the store lacks, as byte ranges, so an
// update costs the bytes that changed rather than every layer whose digest
// did. The saving depends on the previous release's files still being in
// the store: hold that release before pulling, and release it once the new
// one is in place. A layer without a table, or whose table cannot be read,
// is fetched whole, as is one whose missing files would take too many or
// too large ranges; the image built is the same either way. The pull source
// must serve byte ranges, or the pull fails with ErrNoRangeSource; the
// option is not available through NewBuild.
func WithDelta() PullOption {
	return func(o *PullOptions) { o.Delta = true }
}

// MountOptions is the resolved configuration a Mount acts on. Callers do not
// usually build it directly: they pass the With* options to Mount, which apply
// to a zero MountOptions. The zero value yields a read-only composefs mount.
// Writable mounts are not supported: Mount rejects any of Writable,
// OverlayRoot, UpperDir, or WorkDir being set.
type MountOptions struct {
	// Writable requests a writable overlay over the read-only composefs
	// base; Mount rejects it.
	Writable bool
	// OverlayRoot names a base for a writable mount's upperdir and workdir;
	// Mount rejects it.
	OverlayRoot string
	// UpperDir and WorkDir name a writable overlay layer explicitly; Mount
	// rejects them.
	UpperDir string
	WorkDir  string
	// MetadataOnly mounts only the image's EROFS metadata, read-only on a
	// loop device, without the object data plane a composefs mount attaches.
	// The caller assembles its own single overlay over the mount with
	// ObjectsDir as a data-only lower, passing verity=require so fs-verity
	// stays enforced. It cannot be combined with the writable options.
	MetadataOnly bool
}

// MountOption opts a Mount into a particular behaviour. The zero set of options
// yields a read-only composefs mount.
type MountOption func(*MountOptions)

// WithWritable requests a writable overlay over the read-only composefs base.
// Writable mounts are not supported: Mount rejects this option. A caller that
// needs a writable rootfs assembles it over a WithMetadataOnly mount.
func WithWritable() MountOption {
	return func(o *MountOptions) { o.Writable = true }
}

// WithOverlayRoot names a base directory for a writable mount's upperdir and
// workdir. It implies a writable overlay, so Mount rejects it.
func WithOverlayRoot(dir string) MountOption {
	return func(o *MountOptions) {
		o.OverlayRoot = dir
		o.Writable = true
	}
}

// WithUpperDir names a writable overlay's upper directory explicitly. It
// implies a writable overlay, so Mount rejects it.
func WithUpperDir(dir string) MountOption {
	return func(o *MountOptions) {
		o.UpperDir = dir
		o.Writable = true
	}
}

// WithWorkDir names a writable overlay's work directory explicitly. It
// implies a writable overlay, so Mount rejects it.
func WithWorkDir(dir string) MountOption {
	return func(o *MountOptions) {
		o.WorkDir = dir
		o.Writable = true
	}
}

// WithMetadataOnly mounts only the image's verified EROFS metadata, for a
// caller that assembles its own overlay data plane: stacking a writable
// overlay on a composefs mount can exceed the kernel's filesystem
// stacking-depth limit, so such a caller folds the store's object directory
// (ObjectsDir) into its overlay as a data-only lower instead.
func WithMetadataOnly() MountOption {
	return func(o *MountOptions) { o.MetadataOnly = true }
}

// GCOptions is the resolved configuration a GC acts on. Callers pass the
// With* options to GC, which apply to a zero GCOptions.
type GCOptions struct {
	// DryRun reports what a collection would remove without removing it.
	DryRun bool
}

// GCOption adjusts GCOptions.
type GCOption func(*GCOptions)

// WithDryRun makes GC report what it would remove without removing anything.
func WithDryRun() GCOption {
	return func(o *GCOptions) { o.DryRun = true }
}

// GCReport summarizes one garbage collection.
type GCReport struct {
	// RefsRemoved counts runtime references deleted because the holder they
	// named an image for no longer stands on it.
	RefsRemoved int
	// ObjectsRemoved counts store objects deleted, orphan view entries
	// included.
	ObjectsRemoved int
	// BytesFreed sums the deleted objects' sizes. Orphan view entries and
	// bindings are noise-level and add nothing here.
	BytesFreed int64
	// BindingsRemoved counts orphan image bindings deleted.
	BindingsRemoved int
	// InUseSkipped counts objects kept because a mounter holds them.
	InUseSkipped int
	// Removed lists the deleted store objects by content digest. Orphan view
	// entries are counted in ObjectsRemoved but not listed here, so its
	// length can fall short of that count.
	Removed []string
	// Candidates counts the objects judged unwanted before the in-use probe,
	// which InUseSkipped then takes from.
	Candidates int
	// InventoryDuration is the time spent walking and measuring every
	// object under the shared store lock.
	InventoryDuration time.Duration
	// MarkSweepDuration is the time spent under the exclusive store lock
	// marking wanted content, sweeping records, and probing for mounters,
	// up to the first deletion.
	MarkSweepDuration time.Duration
}

// RemoveOptions is the resolved configuration a Remove acts on. Callers pass
// the With* options to Remove, which apply to a zero RemoveOptions.
type RemoveOptions struct {
	// Force removes the image even while holds pin it, clearing all of the
	// image's holds first.
	Force bool
}

// RemoveOption opts a Remove into a particular behaviour.
type RemoveOption func(*RemoveOptions)

// WithForce accepts the force option for Remove.
func WithForce() RemoveOption {
	return func(o *RemoveOptions) { o.Force = true }
}

// Engine performs the ocicfs primitives against a local content-addressed
// store.
type Engine interface {
	// Publish builds the tree at dir into a zstd:chunked OCI image and pushes
	// it to dest. It is not implemented yet and returns ErrNotImplemented.
	Publish(ctx context.Context, dir, dest string) error
	// Pull fetches ref into the local store and returns the stored image. It
	// reuses saved layer file lists and, with WithDelta, range-fetches missing
	// files where that pays; other layers are read whole. Registry resolution
	// requires Config.RegistrySource. A registry ref may be
	// digest-pinned (the digest is the trust anchor) or tagged, in which case
	// the tag is resolved to the digest the registry currently serves and
	// recorded only as a reference alias; a sideload ref must be digest-pinned.
	// A reference landing on an image index is descended, inside
	// the index's own verified bytes, to the manifest it offers for the host
	// platform: that manifest is the image's identity and the digest the
	// returned Image reports, so pulling one image by its index and by its
	// manifest stores it once.
	Pull(ctx context.Context, ref string, opts ...PullOption) (*Image, error)
	// NewBuild starts a pull whose layers are ingested on the caller's cue. It
	// takes the same reference and options as Pull, resolves the reference
	// and verifies the metadata chain up front, and returns a Build to feed
	// layers to; Pull without WithDelta is NewBuild followed by Finish, and
	// NewBuild rejects WithDelta. The store is held against gc from NewBuild
	// until the build's Finish or Close, so gc reports ErrStoreBusy for the
	// build's whole lifetime. Registry resolution requires
	// Config.RegistrySource.
	NewBuild(ctx context.Context, ref string, opts ...PullOption) (Build, error)
	// Mount mounts ref at target: a read-only composefs tree by default — the
	// image's EROFS metadata served through one overlay with ObjectsDir as a
	// data-only lower and fs-verity required — or the image's verified EROFS
	// metadata alone with WithMetadataOnly. Writable mounts are not
	// supported: the writable With* options are rejected. Mount operates on
	// content already present in the store and does not fetch over the
	// network; pull ref first. A ref that
	// does not pin a digest resolves locally to the identity its most recent
	// pull recorded, and a pinned index digest resolves to the manifest it
	// offers for the host platform; every verification keys on that identity.
	Mount(ctx context.Context, ref, target string, opts ...MountOption) error
	// MountChain mounts the stored image whose full layer chain has the given
	// chain identity. Resolution reads only local state and never fetches over
	// the network; a chain identity no stored image ends at is reported as an
	// error satisfying errors.Is(err, ErrNotFound). Verification and mount
	// semantics are Mount's, keyed on the identity the resolution produces.
	MountChain(ctx context.Context, chainID, target string, opts ...MountOption) error
	// Unmount tears down the mount at target.
	Unmount(ctx context.Context, target string) error
	// ObjectsDir returns the store's composefs backing-object directory, for
	// a caller that assembles its own overlay data plane over a metadata-only
	// mount (the directory is the overlay's data-only lower).
	ObjectsDir() (string, error)
	// Inspect reports the OCI identity and composefs binding of an image
	// already present in the local store: identity digest, resolved platform,
	// ordered config diff_ids, and the composefs image descriptor with its
	// fs-verity digest. Inspect reads only local state and never fetches over
	// the network; a ref that does not pin a digest resolves locally to the
	// identity its most recent pull recorded, and a pinned index digest to the
	// manifest that index offers for the host platform. Inspect returns an
	// error satisfying errors.Is(err, ErrNotFound) when the image has not been
	// pulled (its metadata or binding is absent locally). An image whose
	// index does not offer the host platform is a
	// different, permanent condition and is reported as its own error.
	Inspect(ctx context.Context, ref string) (*ImageInfo, error)
	// List returns the images present in the local store, each reporting the
	// identity digest it is stored under.
	List(ctx context.Context) ([]Image, error)
	// Hold pins ref's identity digest under a named holder so a Remove of that
	// image declines while the holder is present. It reads only local state; a
	// ref that does not pin a digest resolves through the most recent pull, a
	// pinned index digest resolves to the manifest it offers for the host
	// platform, and an image not present locally is reported as an error
	// satisfying errors.Is(err, ErrNotFound). A digest whose stored blob does
	// not hash to it is store corruption and is reported as an error rather
	// than held. Recording an already-present holder succeeds without change.
	Hold(ctx context.Context, ref, holder string) error
	// Release drops a named holder's hold on ref's identity digest. It resolves
	// ref the way Hold does but is best-effort about store state: a digest
	// whose blob is absent or damaged releases as given rather than erroring,
	// so a hold can always be dropped. Releasing a holder that is not present
	// succeeds.
	Release(ctx context.Context, ref, holder string) error
	// Holds reports every identity digest at least one holder pins, with the
	// sorted holder names standing on it. It reads only local state and takes
	// no lock, so a hold or release racing it may or may not be reflected.
	Holds(ctx context.Context) (map[digest.Digest][]string, error)
	// Remove forgets a local image: its reference record, and the image
	// binding for its identity digest that no other reference still resolves
	// to. It declines while a hold pins that identity unless WithForce is set,
	// which clears the holds first.
	Remove(ctx context.Context, ref string, opts ...RemoveOption) error
	// GC deletes every store object that is neither wanted — reachable from
	// an agent's reference record or a hold; a runtime reference pins nothing
	// — nor in use by a mounter, along with orphan bindings, runtime
	// references whose holder no longer stands, orphan fs-verity view
	// entries, staging leftovers, and the empty shard directories earlier
	// deletions left behind. It inventories the objects under the shared
	// store lock and decides and deletes under the exclusive one, failing
	// fast with an error matching ErrStoreBusy while a pull, hold, or mount
	// is in progress at the start, or while one started during the
	// inventory is still running when the lock turns exclusive; retry. A
	// wanted image it cannot fully enumerate aborts the collection with
	// nothing deleted, naming the broken root and the remedy. WithDryRun
	// reports the same counts a collection would produce without removing
	// anything, except orphan view entries, which exist only after external
	// damage and are counted only when actually swept.
	GC(ctx context.Context, opts ...GCOption) (*GCReport, error)
	// PreflightTools verifies mkcomposefs, which building images into the
	// store runs, is available on PATH. Pull performs the same check before
	// it needs the tool; PreflightTools lets a caller that will feed builds
	// into the store fail fast, or report the prerequisite on its own,
	// without starting a build.
	PreflightTools() error
	// PreflightStore verifies the store's filesystem can enable fs-verity,
	// which every content object requires. Materialization performs the
	// same probe before writing objects; PreflightStore reports it up
	// front, with the same error, independently of the tools check.
	PreflightStore() error
}

// Config configures a new Engine.
type Config struct {
	// Root is the ocicfs data directory holding the content-addressed store.
	Root string
	// RegistrySource opens a registry reference for Pull and NewBuild when the
	// caller gives no blob source or layout directory. Nil means the engine has
	// no registry: such a pull fails with ErrNoRegistry. The ocicfs CLI sets it;
	// an embedder that feeds blobs itself leaves it nil.
	RegistrySource func(ctx context.Context, ref string) (BlobSource, digest.Digest, error)
	// OverlayRoot is unused because writable mounts are not supported.
	OverlayRoot string
	// IngestWorkers is the number of layers scanned concurrently across all
	// builds of the engine. 0 means DefaultIngestWorkers; a negative value is
	// rejected by New.
	IngestWorkers int
}

// DefaultIngestWorkers is the number of layers an engine scans at once when
// Config.IngestWorkers is 0.
const DefaultIngestWorkers = 2

// engine is the default Engine backed by a local ocicfs data root.
type engine struct {
	root           string
	registrySource func(context.Context, string) (BlobSource, digest.Digest, error)
	overlayRoot    string
	obj            *objectstore.Store
	img            *imagestore.Store
	refs           *refstore.Store
	holds          *holdstore.Store
	slots          chan struct{} // caps the layer scans running at once across all builds
}

// New returns an Engine backed by the store under cfg.Root, creating the
// store's directories on first use.
func New(cfg Config) (Engine, error) {
	if cfg.Root == "" {
		return nil, errors.New("ocicfs: empty root directory")
	}
	workers := cfg.IngestWorkers
	if workers < 0 {
		return nil, fmt.Errorf("ocicfs: ingest workers must not be negative, got %d", workers)
	}
	if workers == 0 {
		workers = DefaultIngestWorkers
	}
	obj, err := objectstore.New(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("ocicfs: open object store: %w", err)
	}
	img, err := imagestore.New(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("ocicfs: open image store: %w", err)
	}
	refs, err := refstore.New(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("ocicfs: open ref store: %w", err)
	}
	holds, err := holdstore.New(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("ocicfs: open hold store: %w", err)
	}
	return &engine{
		root: cfg.Root, registrySource: cfg.RegistrySource, overlayRoot: cfg.OverlayRoot,
		obj: obj, img: img, refs: refs, holds: holds,
		slots: make(chan struct{}, workers),
	}, nil
}
