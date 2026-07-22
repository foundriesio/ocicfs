// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/mount"
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

	root, err := e.resolveLocalRef(ref)
	if err != nil {
		return err
	}
	if err := validateMountTarget(target); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	platform := hostPlatform()
	b, err := e.img.Get(root, platform)
	if errors.Is(err, imagestore.ErrNotFound) {
		return fmt.Errorf("not pulled for this platform; run `knarr pull` first: %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return fmt.Errorf("read image binding: %w", err)
	}

	measured, err := e.obj.VerityDigest(b.Descriptor.Digest)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("image object missing from store; run `knarr pull` first: %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return fmt.Errorf("verify image object %s: %w", b.Descriptor.Digest, err)
	}
	if measured != b.FsverityDigest {
		return fmt.Errorf("image object fs-verity digest mismatch: measured %s, binding records %s", measured, b.FsverityDigest)
	}
	imagePath, err := e.obj.ObjectPath(b.Descriptor.Digest)
	if err != nil {
		return err
	}

	spec := mount.Spec{
		ImagePath:      imagePath,
		Basedir:        filepath.Join(e.root, b.ObjectStorePath),
		Target:         target,
		FsverityDigest: b.FsverityDigest,
	}
	if cfg.MetadataOnly {
		return mount.MetadataOnly(ctx, spec)
	}
	return mount.ReadOnly(ctx, spec)
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
