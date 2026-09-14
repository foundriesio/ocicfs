// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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
// identity chainID, reading only local state. It inspects every image the
// store holds a binding for — a reference is not required, so an image a hold
// alone keeps is found like any other — and returns the first whose final
// layer chain ID equals chainID; images that share a full chain have
// byte-identical rootfs by construction, so the first match is as good as
// any. A zero-layer image has no final chain ID and is never a match. Bindings
// are enumerated by their identity digest alone; an image bound for several
// platforms is inspected once, for the host platform, like any other.
//
// An image that cannot be inspected is skipped and the scan continues, so one
// unreadable or corrupt binding cannot hide a healthy match. When the scan
// ends with no match, the returned error satisfies errors.Is(err, ErrNotFound)
// and joins every inspection error encountered, each naming the image that
// failed, so store corruption stays visible instead of reading as a plain
// miss. A chainID matching only a proper prefix of a chain, or matching
// nothing, is not found.
func (e *engine) resolveChain(ctx context.Context, chainID string) (*ImageInfo, error) {
	want, err := digestutil.ParseSHA256(chainID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var (
		found    *ImageInfo
		scanErrs []error
		seen     = map[digest.Digest]struct{}{}
	)
	err = e.img.Walk(ctx, func(root digest.Digest, _ ocispec.Platform) error {
		if _, ok := seen[root]; ok {
			return nil
		}
		seen[root] = struct{}{}
		info, err := e.inspect(ctx, root.String())
		if err != nil {
			scanErrs = append(scanErrs, fmt.Errorf("inspect %s: %w", root, err))
			return nil
		}
		if n := len(info.Layers); n > 0 && info.Layers[n-1].ChainID == want {
			found = info
			return errChainFound
		}
		return nil
	})
	if found != nil {
		return found, nil
	}
	// A cancelled scan aborts with its context error rather than folding into
	// skip-and-continue, so a caller never reads cancellation as a not-found
	// result; the walk checks the context before every binding.
	if err != nil {
		return nil, err
	}

	errs := []error{fmt.Errorf("no stored image with final chain ID %s: %w", want, ErrNotFound)}
	return nil, errors.Join(append(errs, scanErrs...)...)
}

// errChainFound stops the binding walk at the first match.
var errChainFound = errors.New("chain found")
