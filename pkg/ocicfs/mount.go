// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/mount"
	"github.com/foundriesio/ocicfs/internal/ociseal"
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
	// an orphan binding during gc's inventory, before the image is judged.
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
		return fmt.Errorf("no composefs image for this platform; run `ocicfs pull` first: %w", errors.Join(ErrNotFound, err))
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
		return fmt.Errorf("image object missing from store; run `ocicfs pull` first: %w", errors.Join(ErrNotFound, err))
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
// sealing annotation. The annotation is selected by presence alone, never by
// the binding's canonical flag: the binding is untrusted here, and letting it
// steer the key choice would let a tampered binding present a sealed image as
// unsealed and re-enter binding trust. A sealing annotation that cannot be
// verified fails closed rather than falling back to the binding.
func expectedFsverity(annotations map[string]string, bindingFsverity string) (string, bool, error) {
	key, sealedDigest, err := ociseal.SelectMergedAnnotation(annotations)
	if err != nil {
		return "", false, err
	}
	if key == "" {
		return bindingFsverity, false, nil
	}
	return sealedDigest, true, nil
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
	mounted, err := isMountpoint(target)
	if err != nil {
		return fmt.Errorf("probe target %s for an existing mount: %w", target, err)
	}
	if mounted {
		return fmt.Errorf("target %s already carries a mount; unmount it first: %w", target, ErrAlreadyMounted)
	}
	return nil
}

// isMountpoint reports whether dir is the root of a mount. Mounting on such a
// target would not replace the existing mount but silently stack over it,
// shadowing a mount that stays alive — and in-use — underneath, so the caller
// refuses instead. The probe opens the directory's final path component with
// mount-boundary crossing forbidden: only that last step is restricted, so
// mounts legitimately traversed by the leading path do not trigger it. The
// path is symlink-resolved first, otherwise a link whose expansion traverses
// an unrelated mount would read as mounted. The check is advisory ordering,
// not a lock: a mount racing in after the probe still stacks.
func isMountpoint(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false, err
	}
	if resolved == "/" {
		return true, nil
	}
	parent, err := unix.Open(filepath.Dir(resolved), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: filepath.Dir(resolved), Err: err}
	}
	defer warnCloseFd("mount: failed to close target parent fd", parent)
	fd, err := unix.Openat2(parent, filepath.Base(resolved), &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_XDEV,
	})
	if errors.Is(err, unix.EXDEV) {
		return true, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "openat2", Path: resolved, Err: err}
	}
	warnCloseFd("mount: failed to close target probe fd", fd)
	return false, nil
}

// warnCloseFd closes fd, logging a failure rather than dropping it.
func warnCloseFd(msg string, fd int) {
	if err := unix.Close(fd); err != nil {
		slog.Warn(msg, "err", err)
	}
}
