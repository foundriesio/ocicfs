// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package mount owns read-only composefs and EROFS metadata mount operations.
package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// Spec describes one read-only composefs mount.
type Spec struct {
	ImagePath      string
	Basedir        string
	Target         string
	FsverityDigest string
}

// ErrNotMounted reports that the unmount target had no mount to tear down, so
// callers with idempotent teardown paths can match it with errors.Is.
var ErrNotMounted = errors.New("mount: target is not mounted")

// ReadOnly mounts s.ImagePath at s.Target with fs-verity required.
func ReadOnly(ctx context.Context, s Spec) error {
	opts, err := composefsOptions(s)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "mount.composefs", "-o", opts, "--", s.ImagePath, s.Target) // #nosec G204 -- fixed binary, discrete validated args, "--" ends option parsing
	return runMount(ctx, cmd, "mount.composefs")
}

// MetadataOnly mounts s.ImagePath at s.Target as the raw read-only EROFS
// metadata image, without the object data plane a composefs mount attaches.
// A caller that must fold a writable layer into a single overlay — stacking
// an overlay on a composefs mount can exceed the kernel's filesystem
// stacking-depth limit — mounts the metadata this way and assembles its own
// overlay with the store's object directory as a data-only lower. Only
// ImagePath and Target are consulted; the caller verifies the image's
// fs-verity digest before mounting.
//
// The image is attached to an explicitly read-only loop device rather than
// mounted from the file or through mount(8): a file-backed EROFS counts an
// extra filesystem stacking level, which would push the caller's overlay
// past the kernel's depth limit, and mount(8)'s loop handling opens the
// backing file for writing on some util-linux versions, which an fs-verity
// sealed image refuses.
func MetadataOnly(ctx context.Context, s Spec) error {
	if s.ImagePath == "" {
		return errors.New("mount: empty image path")
	}
	if s.Target == "" {
		return errors.New("mount: empty target")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	img, err := os.Open(s.ImagePath) // #nosec G304 -- store object path, verified by caller
	if err != nil {
		return fmt.Errorf("mount: open image: %w", err)
	}
	defer warnClose("mount: failed to close image", img)

	dev, err := attachLoop(img)
	if err != nil {
		return err
	}
	// The loop device is configured with autoclear: once this handle closes,
	// it detaches when the mount (which holds its own reference) is torn down.
	defer warnClose("mount: failed to close loop device", dev)

	if err := unix.Mount(dev.Name(), s.Target, "erofs", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount: erofs %s on %s: %w", dev.Name(), s.Target, err)
	}
	return nil
}

// attachLoop attaches img to a free loop device, read-only with autoclear,
// and returns the open device. Allocation can race with other loop users, so
// a device that is already busy by configure time is retried with a fresh one.
func attachLoop(img *os.File) (*os.File, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("mount: open loop control: %w", err)
	}
	defer warnClose("mount: failed to close loop control", ctl)

	for range 16 {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE) // #nosec G115 -- a file descriptor always fits in int
		if err != nil {
			return nil, fmt.Errorf("mount: allocate loop device: %w", err)
		}
		path := fmt.Sprintf("/dev/loop%d", n)
		dev, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- kernel-allocated loop device index
		if errors.Is(err, os.ErrNotExist) {
			// A freshly allocated device has no node when /dev is a static
			// tmpfs (a container without udev); create it. A concurrent
			// creator losing the race to mknod is fine — the node is the same.
			if merr := unix.Mknod(path, unix.S_IFBLK|0o600, int(unix.Mkdev(7, uint32(n)))); merr != nil && !errors.Is(merr, os.ErrExist) { // #nosec G302 G115 -- loop device node; kernel loop index fits in uint32
				return nil, fmt.Errorf("mount: create loop device node %s: %w", path, merr)
			}
			dev, err = os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- kernel-allocated loop device index
		}
		if err != nil {
			return nil, fmt.Errorf("mount: open loop device %d: %w", n, err)
		}
		cfg := unix.LoopConfig{
			Fd: uint32(img.Fd()), // #nosec G115 -- a file descriptor always fits in uint32
			Info: unix.LoopInfo64{
				Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR,
			},
		}
		err = unix.IoctlLoopConfigure(int(dev.Fd()), &cfg) // #nosec G115 -- a file descriptor always fits in int
		if err == nil {
			return dev, nil
		}
		warnClose("mount: failed to close loop device", dev)
		if !errors.Is(err, unix.EBUSY) {
			return nil, fmt.Errorf("mount: configure loop device %d: %w", n, err)
		}
	}
	return nil, errors.New("mount: no free loop device after retries")
}

// warnClose closes c, logging a failure rather than dropping it.
func warnClose(msg string, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(msg, "err", err)
	}
}

// runMount runs a prepared mount command, folding its stderr and any context
// cancellation into the returned error.
func runMount(ctx context.Context, cmd *exec.Cmd, tool string) error {
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return commandError(tool, ctxErr, stderr.String())
		}
		return commandError(tool, err, stderr.String())
	}
	return nil
}

// Unmount tears down the mount at target. A target with no mount returns an
// error matching ErrNotMounted.
func Unmount(target string) error {
	if target == "" {
		return errors.New("mount: empty target")
	}
	if err := unix.Unmount(target, 0); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("mount: %s: %w", target, errors.Join(ErrNotMounted, err))
		}
		return fmt.Errorf("mount: unmount %s: %w", target, err)
	}
	return nil
}

// composefsOptions validates s and builds the -o string that carries the trust
// posture: the image's expected fs-verity digest, fs-verity required on every
// redirected object, and a read-only mount.
func composefsOptions(s Spec) (string, error) {
	if err := validateSpec(s); err != nil {
		return "", err
	}
	digestHex, err := fsverityDigestHex(s.FsverityDigest)
	if err != nil {
		return "", err
	}
	basedir, err := optionValue("basedir", s.Basedir)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"basedir=" + basedir,
		"digest=" + digestHex,
		"verity",
		"ro",
	}, ","), nil
}

func validateSpec(s Spec) error {
	switch {
	case s.ImagePath == "":
		return errors.New("mount: empty image path")
	case s.Basedir == "":
		return errors.New("mount: empty basedir")
	case s.Target == "":
		return errors.New("mount: empty target")
	case s.FsverityDigest == "":
		return errors.New("mount: empty fs-verity digest")
	}
	return nil
}

func fsverityDigestHex(raw string) (string, error) {
	d, err := digest.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("mount: fs-verity digest: %w", err)
	}
	if d.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("mount: fs-verity digest: non-sha256 digest algorithm %s", d.Algorithm())
	}
	if err := d.Validate(); err != nil {
		return "", fmt.Errorf("mount: fs-verity digest: %w", err)
	}
	return d.Hex(), nil
}

// optionValue returns value for use in a mount.composefs -o option, rejecting
// the delimiter bytes that would split it: ',' separates options and ':'
// separates multiple basedirs.
func optionValue(name, value string) (string, error) {
	if strings.ContainsAny(value, ":,") {
		return "", fmt.Errorf("mount: %s cannot contain ':' or ','", name)
	}
	return value, nil
}

func commandError(tool string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("mount: %s: %w: %s", tool, err, stderr)
	}
	return fmt.Errorf("mount: %s: %w", tool, err)
}
