// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/mount"
	"github.com/foundriesio/knarr/internal/ociseal"
)

const writableMountUnsupported = "writable mounts are not supported"

// Mount mounts ref at target.
func (e *engine) Mount(ctx context.Context, ref, target string, opts ...MountOption) error {
	if err := e.mount(ctx, ref, target, opts...); err != nil {
		return fmt.Errorf("mount %s on %s: %w", ref, target, err)
	}
	return nil
}

// ObjectsDir returns the store's fs-verity view directory, the data plane a
// merged image's redirects resolve through.
func (e *engine) ObjectsDir() (string, error) {
	return e.obj.ViewDir(), nil
}

// Unmount tears down the mount at target. A target with no mount returns an
// error matching ErrNotMounted.
func (e *engine) Unmount(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	if err := mount.Unmount(target); err != nil {
		if errors.Is(err, mount.ErrNotMounted) {
			err = errors.Join(ErrNotMounted, err)
		}
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

func (e *engine) mount(ctx context.Context, ref, target string, opts ...MountOption) error {
	var cfg MountOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.Writable || cfg.OverlayRoot != "" || cfg.UpperDir != "" || cfg.WorkDir != "" {
		return errors.New(writableMountUnsupported)
	}

	// The store lock is held shared across the whole mount setup: a
	// digest-pinned mount needs no ref record, so it can otherwise begin from
	// an orphan binding after gc's census judged the image unwanted and idle.
	// Under the shared lock no mount overlaps a sweep; once the image's own
	// in-use lock is taken inside the mount, the sweep sees it.
	unlock, err := e.lockStoreShared()
	if err != nil {
		return err
	}
	defer unlock()

	root, _, err := e.localAnchor(ref)
	if root == "" {
		return err
	}
	if err := validateMountTarget(target); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Read and hash-verify the stored metadata chain before anything else; the
	// binding and object lookups below key on the verified identity it yields.
	res, err := e.resolveStoredImage(ctx, root, hostPlatform())
	if err != nil {
		return err
	}

	b, err := e.img.Get(res.ManifestDesc.Digest, res.Platform)
	if errors.Is(err, imagestore.ErrNotFound) {
		return fmt.Errorf("no composefs image for this platform; run `knarr pull` first: %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return fmt.Errorf("read image binding: %w", err)
	}

	expected, sealed, err := expectedFsverity(res.Manifest.Annotations, b.FsverityDigest)
	if err != nil {
		return err
	}

	measured, err := e.obj.VerityDigest(b.Descriptor.Digest)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("image object missing from store; run `knarr pull` first: %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return fmt.Errorf("verify image object %s: %w", b.Descriptor.Digest, err)
	}
	if measured != expected {
		if sealed {
			return fmt.Errorf("image object fs-verity digest mismatch: measured %s, sealing annotation claims %s; remove the image and pull again, or re-seal it", measured, expected)
		}
		return fmt.Errorf("image object fs-verity digest mismatch: measured %s, binding records %s", measured, expected)
	}
	imagePath, err := e.obj.ObjectPath(b.Descriptor.Digest)
	if err != nil {
		return err
	}

	spec := mount.Spec{
		ImagePath:      imagePath,
		Basedir:        filepath.Join(e.root, b.ObjectStorePath),
		Target:         target,
		FsverityDigest: expected,
	}
	if cfg.MetadataOnly {
		return mount.MetadataOnly(ctx, spec)
	}
	return mount.ReadOnly(ctx, spec)
}

// expectedFsverity selects the fs-verity digest a mount enforces from the
// verified identity-manifest annotations, falling back to the binding's
// locally measured digest only when the manifest carries no merged-image
// sealing annotation. Selection is by annotation presence alone, never by the
// binding's canonical flag: the binding is untrusted here, and letting it
// steer the key choice would let a tampered binding present a sealed image as
// unsealed and re-enter binding trust. The standard key wins over the knarr
// fallback key when both are present. A malformed value and a merged-image
// annotation of an unrecognized digest kind both fail closed: the publisher
// sealed the image, so falling back to the binding would silently drop the
// seal. An unrecognized digest kind coexisting with a recognized key does
// not fail: every annotation sits inside the hash-pinned manifest, so the
// combination is publisher-authored — dual-sealing for a format migration —
// not a downgrade an attacker can induce, and the recognized seal pins the
// content at full strength on its own.
func expectedFsverity(annotations map[string]string, bindingFsverity string) (string, bool, error) {
	for _, key := range []string{ociseal.MergedAnnotation, ociseal.FallbackMergedAnnotation} {
		value, ok := annotations[key]
		if !ok {
			continue
		}
		sealedDigest, err := ociseal.ParseAnnotation(value)
		if err != nil {
			return "", false, fmt.Errorf("sealing annotation %s: %w", key, err)
		}
		return sealedDigest, true, nil
	}
	for key := range annotations {
		if strings.HasPrefix(key, ociseal.MergedAnnotationPrefix) {
			return "", false, fmt.Errorf("unverifiable sealing annotation %q: unrecognized digest kind", key)
		}
	}
	return bindingFsverity, false, nil
}

func validateMountTarget(target string) error {
	fi, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("target %s does not exist: %w", target, err)
	}
	if err != nil {
		return fmt.Errorf("stat target %s: %w", target, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("target %s is not a directory", target)
	}
	return nil
}
