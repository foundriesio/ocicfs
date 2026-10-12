// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/ocichain"
)

// Hold pins ref's identity digest under holder so a remove of that image
// declines while the holder is present.
func (e *engine) Hold(ctx context.Context, ref, holder string) error {
	if err := e.hold(ctx, ref, holder); err != nil {
		return fmt.Errorf("hold %s: %w", ref, err)
	}
	return nil
}

// Release drops holder's hold on ref's identity digest.
func (e *engine) Release(ctx context.Context, ref, holder string) error {
	if err := e.release(ctx, ref, holder); err != nil {
		return fmt.Errorf("release %s: %w", ref, err)
	}
	return nil
}

// Holds reports every held identity digest and the holders standing on it.
func (e *engine) Holds(ctx context.Context) (map[digest.Digest][]string, error) {
	held, err := e.holds.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list holds: %w", err)
	}
	out := make(map[digest.Digest][]string, len(held))
	for _, root := range held {
		holders, err := e.holds.Holders(root)
		if err != nil {
			return nil, fmt.Errorf("list holds: %w", err)
		}
		// A last release racing the listing empties the digest between the
		// two reads; it is then not held.
		if len(holders) > 0 {
			out[root] = holders
		}
	}
	return out, nil
}

func (e *engine) hold(ctx context.Context, ref, holder string) error {
	// The store lock is held shared across the whole hold: presence is checked
	// before the marker is written, and a concurrent sweep between the two
	// would strand the holder on absent content. It nests outside the
	// per-digest lock below; gc takes only the store lock, so the order cannot
	// deadlock.
	unlockStore, err := e.lockStoreShared()
	if err != nil {
		return err
	}
	defer unlockStore()

	root, err := e.resolveLocalRef(ctx, ref)
	if root == "" {
		return err
	}
	// A blob that does not hash to the digest naming it is store corruption, and
	// a hold recorded under a digest nothing checks would be false protection:
	// that is the one resolution failure a hold refuses. Every other one holds
	// the digest as given — an absent blob reaches the presence check below and
	// is reported as not pulled, while a verified blob that is not an OCI index
	// (a loose content object, say) is held as the object it is, its presence
	// decided by the object store.
	if errors.Is(err, ocichain.ErrDigestMismatch) {
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
	// A release is best-effort about store state: whatever the descent made of
	// the digest — absent, corrupt, unreadable — the holder is dropped from the
	// digest as given. A holder that could not release would linger forever,
	// and the callers that release during teardown only warn.
	root, err := e.resolveLocalRef(ctx, ref)
	if root == "" {
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
// object store. A hold is keyed by the identity digest and protects content
// identity, so presence is decided digest-wide by that blob in the CAS, not by
// the auxiliary, regenerable, per-host-platform binding. An absent root is
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
