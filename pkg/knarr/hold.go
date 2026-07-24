// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"fmt"

	"github.com/opencontainers/go-digest"
)

// Hold pins ref's root digest under holder so a remove of that image declines
// while the holder is present.
func (e *engine) Hold(ctx context.Context, ref, holder string) error {
	if err := e.hold(ctx, ref, holder); err != nil {
		return fmt.Errorf("hold %s: %w", ref, err)
	}
	return nil
}

// Release drops holder's hold on ref's root digest.
func (e *engine) Release(ctx context.Context, ref, holder string) error {
	if err := e.release(ctx, ref, holder); err != nil {
		return fmt.Errorf("release %s: %w", ref, err)
	}
	return nil
}

func (e *engine) hold(ctx context.Context, ref, holder string) error {
	root, err := e.resolveLocalRef(ref)
	if err != nil {
		return err
	}
	unlock, err := e.holds.Lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	// Cancellation is checked here, under the lock, not before it: the lock wait
	// is not interruptible, and this is the single point past which the operation
	// mutates, so a hold whose caller canceled must stop here and not write.
	if err := ctx.Err(); err != nil {
		return err
	}

	// Check presence under the lock so the check and the marker write are one
	// atomic step: a holder is never recorded for content absent from the store.
	if err := e.ensurePresent(root); err != nil {
		return err
	}
	return e.holds.Hold(root, holder)
}

func (e *engine) release(ctx context.Context, ref, holder string) error {
	root, err := e.resolveLocalRef(ref)
	if err != nil {
		return err
	}
	unlock, err := e.holds.Lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.holds.Release(root, holder)
}

// ensurePresent reports whether root's content is present in the authoritative
// object store. A hold is keyed by the root digest and protects content
// identity, so presence is decided digest-wide by the root blob in the CAS, not
// by the auxiliary, regenerable, per-host-platform binding. An absent root is
// reported as ErrNotFound.
func (e *engine) ensurePresent(root digest.Digest) error {
	present, err := e.obj.Has(root)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("not pulled; run `knarr pull` first: %w", ErrNotFound)
	}
	return nil
}
