// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"

	"github.com/foundriesio/knarr/internal/digestutil"
)

// MountChain mounts the stored image whose full layer chain has the given chain
// identity at target.
func (e *engine) MountChain(ctx context.Context, chainID, target string, opts ...MountOption) error {
	if err := e.mountChain(ctx, chainID, target, opts...); err != nil {
		return fmt.Errorf("mount chain %s on %s: %w", chainID, target, err)
	}
	return nil
}

func (e *engine) mountChain(ctx context.Context, chainID, target string, opts ...MountOption) error {
	info, err := e.resolveChain(ctx, chainID)
	if err != nil {
		return err
	}
	// Re-enter the reference-based mount path keyed on the resolved root digest;
	// it re-reads the binding and enforces fs-verity exactly as a plain mount does.
	return e.mount(ctx, info.Root.String(), target, opts...)
}

// resolveChain finds the stored image whose full layer chain ends at the chain
// identity chainID, reading only local state. It inspects each stored reference
// record and returns the first image whose final layer chain ID equals chainID;
// images that share a full chain have byte-identical rootfs by construction, so
// the first match is as good as any. A zero-layer image has no final chain ID
// and is never a match. Records are enumerated by their identity digest alone;
// their platform strings are not parsed here, so a record with an unparseable
// platform is inspected like any other rather than aborting the scan.
//
// An image that cannot be inspected is skipped and the scan continues, so one
// unreadable or corrupt record cannot hide a healthy match. When the scan ends
// with no match, the returned error satisfies errors.Is(err, ErrNotFound) and
// joins every inspection error encountered, each naming the record that failed,
// so store corruption stays visible instead of reading as a plain miss. A
// chainID matching only a proper prefix of a chain, or matching nothing, is not
// found.
func (e *engine) resolveChain(ctx context.Context, chainID string) (*ImageInfo, error) {
	want, err := digestutil.ParseSHA256(chainID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := e.refs.List()
	if err != nil {
		return nil, err
	}

	var scanErrs []error
	for _, rec := range records {
		// A cancelled scan aborts with its context error rather than folding
		// into skip-and-continue, so a caller never reads cancellation as a
		// not-found result.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := e.inspect(ctx, rec.Identity.Digest.String())
		if err != nil {
			scanErrs = append(scanErrs, fmt.Errorf("inspect %s: %w", rec.Identity.Digest, err))
			continue
		}
		if n := len(info.Layers); n > 0 && info.Layers[n-1].ChainID == want {
			return info, nil
		}
	}

	errs := []error{fmt.Errorf("no stored image with final chain ID %s: %w", want, ErrNotFound)}
	return nil, errors.Join(append(errs, scanErrs...)...)
}
