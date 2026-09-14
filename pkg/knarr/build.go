// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/holdstore"
	"github.com/foundriesio/knarr/internal/materialize"
)

// build is the Build an engine hands out: the state of one pull between
// NewBuild and its Finish or Close. It holds the shared store lock for that
// whole span and keeps no context of its own.
//
// Ingest and Finish hold mu for their whole call, so calls queue behind one
// another, and the store lock is released only by the call that finishes
// the build or by Close, never while a call is in progress. Close does not
// queue: it stops the materialize build first, which interrupts a blocked
// call and waits for a Finish that is already writing, and only then takes
// mu to release the lock.
type build struct {
	e        *engine
	ref      string
	desc     descended
	fetched  []byte // root bytes the descent fetched, stored by the cached tail
	platform ocispec.Platform
	// Exactly one of cached and mat is set: cached when the store already held
	// the image at NewBuild, mat otherwise.
	cached *cachedImage
	mat    *materialize.Build
	// hold is the holder the finished image is held under in place of a
	// reference record; empty records the reference.
	hold string

	unlock     func()
	unlockOnce sync.Once

	mu sync.Mutex
	// err is the first failure, or errBuildClosed once the build finished or
	// was closed; every later call returns it.
	err error
}

// errBuildClosed reports a call on a build that has finished or been closed.
var errBuildClosed = errors.New("build is closed")

// NewBuild starts a pull whose layers are ingested on the caller's cue.
func (e *engine) NewBuild(ctx context.Context, ref string, opts ...PullOption) (Build, error) {
	b, err := e.newBuild(ctx, ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	return b, nil
}

func (e *engine) newBuild(ctx context.Context, ref string, opts ...PullOption) (b *build, err error) {
	var cfg PullOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	// A holder name the hold store would reject fails here, before any lock
	// is taken or byte fetched, rather than at the end of the download.
	if cfg.Hold != "" {
		if err := holdstore.ValidateHolder(cfg.Hold); err != nil {
			return nil, err
		}
	}

	// The store lock is held shared from here until the build finishes or is
	// closed: objects are written before the ref record or hold that makes
	// them wanted, and a concurrent sweep in that window would eat the staged
	// image. The wait is not interruptible; the ctx gate right after is the
	// single pre-mutation cancellation check. A failure before the caller has
	// a handle to close releases the lock here, since nothing else can.
	unlock, err := e.lockStoreShared()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	src, root, err := pullSource(ctx, ref, cfg)
	if err != nil {
		return nil, err
	}

	platform := hostPlatform()
	// Descend the pinned root to the image identity before anything is looked up
	// or recorded, so an index-rooted and a manifest-rooted pull of the same
	// image converge on one key.
	desc, fetched, err := e.pullDescent(ctx, src, root, platform)
	if err != nil {
		return nil, err
	}
	b = &build{e: e, ref: ref, desc: desc, fetched: fetched, platform: platform, hold: cfg.Hold, unlock: unlock}

	cached, ok, err := verifiedPulledImage(ctx, e.obj, e.img, desc.Identity, platform)
	if err != nil {
		return nil, err
	}
	if ok {
		b.cached = &cached
		return b, nil
	}

	// Finish only reaches mkcomposefs after every layer is downloaded and
	// unpacked; check it here so a missing binary fails in seconds, not after
	// the whole download. The cached path above never builds an image and
	// deliberately stays usable without mkcomposefs.
	if err := composefs.Preflight(); err != nil {
		return nil, err
	}
	mat, err := materialize.Begin(ctx, e.obj, e.img, src, root, platform, e.slots)
	if err != nil {
		return nil, err
	}
	b.mat = mat
	return b, nil
}

// Ingest blocks until every layer up to and including i is materialized; a
// failure of any layer scanned so far is reported here and by every later
// call. A cancelled ctx returns its error and leaves the build running, so the
// call may be repeated.
func (b *build) Ingest(ctx context.Context, i int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if b.mat == nil {
		// The store already holds the image: there is nothing to materialize.
		return nil
	}
	err := b.mat.Ingest(ctx, i)
	if err == nil {
		return nil
	}
	// The caller's cancellation interrupts the wait and nothing else: the
	// scans go on, so it is not the build's failure.
	if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
		return fmt.Errorf("pull %s: %w", b.ref, err)
	}
	b.fail(err)
	return b.err
}

// Finish ingests any remaining layers, records the image, and returns it. It
// drains every scan before returning, on every path including a build that
// failed earlier, and only then releases the store lock: the lock is never
// dropped while a scan can still write to the store, nor between the image
// being materialized and its reference being recorded. A Close arriving
// while Finish runs interrupts a wait but cannot release the lock before
// Finish returns; a Close that came first ends Finish with errBuildClosed
// before anything is recorded.
func (b *build) Finish(ctx context.Context) (*Image, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.release()
	if b.err != nil {
		// A failed build is spent, but its scans may still be running: stop
		// them before the lock drops. The failure to report is recorded already.
		if b.mat != nil {
			_ = b.mat.Close()
		}
		return nil, b.err
	}
	img, err := b.finish(ctx)
	if err != nil {
		b.fail(err)
		return nil, b.err
	}
	b.fail(errBuildClosed)
	return img, nil
}

// Close abandons an unfinished build: it cancels every scan and waits for
// them, which interrupts a blocked Ingest or Finish, then releases the store
// lock once that call has returned. A Finish already writing its image
// completes and records it first, so an image that was materialized is never
// stranded. A layer failure the build had recorded stays its error;
// otherwise every later call returns errBuildClosed. Idempotent, and a no-op
// after Finish.
func (b *build) Close() {
	var cerr error
	if b.mat != nil {
		cerr = b.mat.Close()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if cerr != nil {
		b.fail(cerr)
	}
	b.fail(errBuildClosed)
	b.release()
}

// finish runs the tail of the pull: the built image's binding and reference
// (or hold), or the reference (or hold) alone for an image the store already
// held. The caller holds mu.
func (b *build) finish(ctx context.Context) (*Image, error) {
	if b.mat == nil {
		return b.finishCached(ctx)
	}
	built, err := b.mat.Finish(ctx)
	if err != nil {
		return nil, err
	}
	holder, err := b.want(built.RootDesc, built.Identity, built.Platform)
	if err != nil {
		return nil, err
	}
	return &Image{
		Ref:    b.ref,
		Digest: built.Identity.Digest.String(),
		Size:   built.Descriptor.Size,
		Holder: holder,
	}, nil
}

// finishCached records the reference, or takes the hold, for an image the
// store already held.
func (b *build) finishCached(ctx context.Context) (*Image, error) {
	// Store the root the descent had to fetch: the identity is already local
	// on this path, but the document the reference resolved to is not, and
	// the record about to be written names it. Sync before the record so no
	// ref points at a blob a crash could lose.
	if b.fetched != nil {
		if err := b.e.obj.WriteBlob(ctx, b.desc.Root.Digest, bytes.NewReader(b.fetched)); err != nil {
			return nil, err
		}
		if err := b.e.obj.Sync(); err != nil {
			return nil, err
		}
	}
	holder, err := b.want(b.desc.Root, b.desc.Identity, b.cached.platform)
	if err != nil {
		return nil, err
	}
	return &Image{
		Ref:    b.ref,
		Digest: b.desc.Identity.Digest.String(),
		Size:   b.cached.descriptor.Size,
		Holder: holder,
	}, nil
}

// want makes the finished image wanted: a reference record by default, or,
// when the build was opened with a holder, a runtime reference naming the
// image for that holder and then the hold itself. It returns the holder the
// image is named under, empty when it is not named: by default, or when an
// agent's reference of the same name stood. Either way it runs after the
// binding is written and while the store lock is still held, so the image
// is never in the store without something that keeps gc off it.
//
// The whole step runs under the identity's hold lock, which a remove takes
// too, so a remove of the image cannot interleave with it. First the binding
// is checked to still be there: a remove of another reference to the same
// image, landing since the build found or wrote the binding, deletes it as
// unshared, and a reference or hold recorded over a missing binding is a
// wanted image gc cannot enumerate, which aborts every collection — so the
// pull fails instead, and a retry rebuilds the binding. Then the name goes
// before the hold, so a failure between the two leaves only a runtime
// reference whose holder never came to stand, which gc sweeps, never a hold
// nothing names or releases. The hold lock nests inside the store lock, the
// order every hold takes, and the record write's own lock nests inside the
// hold lock; nothing takes the two in the other order.
func (b *build) want(root, identity ocispec.Descriptor, platform ocispec.Platform) (string, error) {
	unlock, err := b.e.holds.Lock(identity.Digest)
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := b.e.img.Get(identity.Digest, platform); err != nil {
		return "", fmt.Errorf("image binding was removed while the pull ran; retry: %w", err)
	}
	if b.hold == "" {
		return "", b.e.recordRef(b.ref, root, identity, platform)
	}
	named, err := b.e.recordRuntimeRef(b.ref, root, identity, platform, b.hold)
	if err != nil {
		return "", err
	}
	if err := b.e.holds.Hold(identity.Digest, b.hold); err != nil {
		return "", err
	}
	if !named {
		return "", nil
	}
	return b.hold, nil
}

// fail records err, wrapped with the pull's reference, as the build's sticky
// error unless one is recorded already. A call the materialize build reports
// as interrupted by Close is recorded as this build's own closed state. The
// caller holds mu.
func (b *build) fail(err error) {
	if b.err != nil {
		return
	}
	if errors.Is(err, materialize.ErrClosed) {
		err = errBuildClosed
	}
	b.err = fmt.Errorf("pull %s: %w", b.ref, err)
}

// release drops the store lock exactly once across Finish and Close.
func (b *build) release() {
	b.unlockOnce.Do(b.unlock)
}
