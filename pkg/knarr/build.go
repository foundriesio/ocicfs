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
	"github.com/foundriesio/knarr/internal/materialize"
)

// build is the Build an engine hands out: the state of one pull between
// NewBuild and its Finish or Close. It holds the shared store lock for that
// whole span and keeps no context of its own.
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

	// The store lock is held shared from here until the build finishes or is
	// closed: objects are written before the ref record that makes them
	// wanted, and a concurrent sweep in that window would eat the staged
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
	b = &build{e: e, ref: ref, desc: desc, fetched: fetched, platform: platform, unlock: unlock}

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
	mat, err := materialize.Begin(ctx, e.obj, e.img, src, root, platform)
	if err != nil {
		return nil, err
	}
	b.mat = mat
	return b, nil
}

// Ingest materializes every layer up to and including i not yet materialized.
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
	if err := b.mat.Ingest(ctx, i); err != nil {
		b.fail(err)
		return b.err
	}
	return nil
}

// Finish ingests any remaining layers, records the image, and returns it. The
// store lock is released whether or not it succeeds.
func (b *build) Finish(ctx context.Context) (*Image, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer b.release()
	if b.err != nil {
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

// Close abandons an unfinished build and releases the store lock. It takes
// the build's mutex like any other call, so it waits for a call in progress
// to return rather than interrupting it.
func (b *build) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail(errBuildClosed)
	if b.mat != nil {
		b.mat.Close()
	}
	b.release()
}

// finish runs the tail of the pull: the built image's binding and reference,
// or the reference alone for an image the store already held.
func (b *build) finish(ctx context.Context) (*Image, error) {
	if b.mat == nil {
		return b.finishCached(ctx)
	}
	built, err := b.mat.Finish(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.e.recordRef(b.ref, built.RootDesc, built.Identity, built.Platform); err != nil {
		return nil, err
	}
	return &Image{
		Ref:    b.ref,
		Digest: built.Identity.Digest.String(),
		Size:   built.Descriptor.Size,
	}, nil
}

// finishCached records the reference for an image the store already held.
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
	if err := b.e.recordRef(b.ref, b.desc.Root, b.desc.Identity, b.cached.platform); err != nil {
		return nil, err
	}
	return &Image{
		Ref:    b.ref,
		Digest: b.desc.Identity.Digest.String(),
		Size:   b.cached.descriptor.Size,
	}, nil
}

// fail records err, wrapped with the pull's reference, as the build's sticky
// error unless one is recorded already. The caller holds mu.
func (b *build) fail(err error) {
	if b.err == nil {
		b.err = fmt.Errorf("pull %s: %w", b.ref, err)
	}
}

// release drops the store lock exactly once across Finish and Close.
func (b *build) release() {
	b.unlockOnce.Do(b.unlock)
}
