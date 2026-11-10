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
	"strings"

	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/digestutil"
	"github.com/foundriesio/ocicfs/internal/verity"
)

// Spec describes one read-only composefs mount.
type Spec struct {
	ImagePath      string
	Basedir        string
	Target         string
	FsverityDigest string
	// NoVerity mounts without fs-verity: the image's fs-verity digest is
	// computed from its bytes in userspace instead of measured by the
	// kernel, and the overlay requests no fs-verity validation of the
	// backing objects.
	NoVerity bool
}

// ErrNotMounted reports that the unmount target had no mount to tear down, so
// callers with idempotent teardown paths can match it with errors.Is.
var ErrNotMounted = errors.New("mount: target is not mounted")

// runDir hosts the transient per-mount scratch directories. It is on tmpfs, so
// any residue a hard crash leaves behind clears at reboot, and ocicfs controls
// every byte of the path, which keeps the overlay option string free of the
// delimiter bytes an environment-chosen directory could carry.
const runDir = "/run/ocicfs"

// overlaySource is the filesystem source name the composefs overlay mounts are
// registered under, so /proc/mounts identifies them.
const overlaySource = "composefs"

// loopBlockSize is the loop device's logical block size: the EROFS block size,
// which direct I/O on 4 KiB-sector backing storage needs.
const loopBlockSize = 4096

// ReadOnly mounts s.ImagePath at s.Target as a read-only composefs tree: the
// image's EROFS metadata served through a single overlay whose data-only lower
// layer is s.Basedir, with fs-verity required on every backing object unless
// s.NoVerity is set.
//
// The image is verified and attached exactly as on the metadata-only path, so
// the shared in-use lock rides the loop device and lives as long as the mount.
// The EROFS is mounted on a private scratch directory only long enough for the
// overlay mount to take its own references to it; the scratch mountpoint is
// then lazily detached and removed, leaving the overlay at s.Target as the one
// visible mount, torn down by a single Unmount(s.Target).
func ReadOnly(ctx context.Context, s Spec) error {
	if err := validateSpec(s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	dev, err := attachVerified(ctx, s)
	if err != nil {
		return err
	}
	defer warnClose("mount: failed to close loop device", dev)

	scratch, err := scratchDir()
	if err != nil {
		return err
	}
	// noacl: the image carries no POSIX ACL xattrs, so disable ACL lookups on
	// the metadata tree rather than paying for misses on every access.
	if err := unix.Mount(dev.Name(), scratch, "erofs", unix.MS_RDONLY, "noacl"); err != nil {
		removeDir(scratch)
		return fmt.Errorf("mount: erofs %s on %s: %w", dev.Name(), scratch, err)
	}

	// MS_RDONLY is explicit: an overlay without an upper layer is read-only
	// only as a side effect, and this mount's contract is read-only.
	mountErr := unix.Mount(overlaySource, s.Target, "overlay", unix.MS_RDONLY, overlayOptions(scratch, s.Basedir, s.NoVerity))
	if mountErr != nil {
		mountErr = fmt.Errorf("mount: overlay on %s: %w", s.Target, mountErr)
	}

	// Whether or not the overlay mount succeeded, lazily detach the scratch
	// mountpoint and remove its directory. On success the overlay holds its
	// own references to the EROFS, keeping it — and with it the loop device
	// and the in-use lock — alive until the overlay is unmounted, when the
	// loop autoclear fires; on failure the detach releases them all. A hard
	// kill between the scratch mount above and this detach leaves the scratch
	// mount, its loop device, and the lock behind until reboot: tmpfs-bounded
	// residue that over-retains store objects for one boot, never data loss.
	var detachErr error
	if err := unix.Unmount(scratch, unix.MNT_DETACH); err != nil {
		detachErr = fmt.Errorf("mount: detach scratch mount %s: %w", scratch, err)
	} else {
		removeDir(scratch)
	}

	if mountErr != nil {
		return errors.Join(mountErr, detachErr)
	}
	if detachErr != nil {
		// A successful mount leaves no scratch residue: a surviving scratch
		// mount would keep the EROFS, the loop device, and the in-use lock
		// alive past the target's unmount, until reboot. Unwind the overlay
		// and report the detach failure instead. The unwind detaches lazily,
		// so a racing opener of the just-visible mount cannot keep the failed
		// mount in the namespace; if even that fails, the failure is joined
		// into the returned error rather than leaving a mount the caller was
		// told does not exist.
		if err := unix.Unmount(s.Target, unix.MNT_DETACH); err != nil {
			return errors.Join(detachErr, fmt.Errorf("mount: unwind overlay on %s: %w", s.Target, err))
		}
		return detachErr
	}
	return nil
}

// MetadataOnly mounts s.ImagePath at s.Target as the raw read-only EROFS
// metadata image, without the object data plane a composefs mount attaches.
// A caller that must fold a writable layer into a single overlay — stacking
// an overlay on a composefs mount can exceed the kernel's filesystem
// stacking-depth limit — mounts the metadata this way and assembles its own
// overlay with the store's object directory as a data-only lower.
//
// Basedir is not consulted; s.FsverityDigest is enforced on the very file
// descriptor the loop device attaches, so the mounted bytes are the measured
// bytes even if the pathname is swapped concurrently. A raw EROFS mount has
// no in-kernel digest option, which makes this measurement the enforcement
// point on this path. With s.NoVerity the digest is computed from the
// descriptor's bytes in userspace rather than measured.
func MetadataOnly(ctx context.Context, s Spec) error {
	if s.ImagePath == "" {
		return errors.New("mount: empty image path")
	}
	if s.Target == "" {
		return errors.New("mount: empty target")
	}
	if s.FsverityDigest == "" {
		return errors.New("mount: empty fs-verity digest")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	dev, err := attachVerified(ctx, s)
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

// attachVerified opens and locks the image at s.ImagePath, requires its
// fs-verity digest to equal s.FsverityDigest, and attaches it to a read-only
// autoclear loop device, which it returns. The digest is the kernel's
// measurement, or with s.NoVerity the userspace digest of the locked
// descriptor's bytes. The loop device retains the locked open file
// description: the shared in-use lock lives exactly as long as the loop
// device — and, with autoclear, as long as the mount built on it — surviving
// this process, and the digest is enforced on the very descriptor the device
// serves, so the mounted bytes are the measured bytes even if the pathname is
// swapped concurrently.
func attachVerified(ctx context.Context, s Spec) (*os.File, error) {
	img, err := lockImage(s.ImagePath)
	if err != nil {
		return nil, err
	}
	defer warnClose("mount: failed to close image", img)

	var measured string
	if s.NoVerity {
		measured, err = verity.Digest(ctx, img)
	} else {
		measured, err = verity.Measure(img)
	}
	if err != nil {
		return nil, fmt.Errorf("mount: measure image: %w", err)
	}
	if measured != s.FsverityDigest {
		return nil, fmt.Errorf("mount: image fs-verity digest mismatch: measured %s, expected %s", measured, s.FsverityDigest)
	}
	return attachLoop(img)
}

// lockImage opens the image and takes the store's in-use coordination on it:
// a shared flock marking the image mounted, then a link-count check. Garbage
// collection deletes an object only while holding the exclusive lock on it
// and probes with LOCK_NB, so once the shared lock is held a named inode
// cannot lose its name to collection; an inode that already has no name was
// collected between the open and the lock, and the mount refuses it.
func lockImage(path string) (*os.File, error) {
	img, err := os.Open(path) // #nosec G304 -- store object path, verified by caller
	if err != nil {
		return nil, fmt.Errorf("mount: open image: %w", err)
	}
	if err := lockLinked(img); err != nil {
		warnClose("mount: failed to close image", img)
		return nil, err
	}
	return img, nil
}

// lockLinked takes the shared in-use lock on img and requires its inode to
// still carry a name.
func lockLinked(img *os.File) error {
	if err := unix.Flock(int(img.Fd()), unix.LOCK_SH); err != nil { // #nosec G115 -- a file descriptor always fits in int
		return fmt.Errorf("mount: lock image: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(img.Fd()), &st); err != nil { // #nosec G115 -- a file descriptor always fits in int
		return fmt.Errorf("mount: stat image: %w", err)
	}
	if st.Nlink == 0 {
		return errors.New("mount: image removed by gc; run `ocicfs pull` first")
	}
	return nil
}

// attachLoop attaches img to a free loop device, read-only with autoclear and
// direct I/O, and returns the open device. Direct I/O with 4 KiB logical
// blocks (the EROFS block size) stops the image's pages from being cached a
// second time under the loop device. The kernel reads buffered anyway when
// the image has fs-verity enabled or its filesystem lacks direct I/O.
// Allocation can race with other loop users, so a device that is already busy
// by configure time is retried with a fresh one.
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
			Fd:   uint32(img.Fd()), // #nosec G115 -- a file descriptor always fits in uint32
			Size: loopBlockSize,
			Info: unix.LoopInfo64{
				Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_DIRECT_IO | unix.LO_FLAGS_AUTOCLEAR,
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

// scratchDir creates a fresh private mountpoint under runDir, creating runDir
// itself on first use.
func scratchDir() (string, error) {
	// Owner-only: the scratch mountpoints are touched exclusively by the
	// privileged ocicfs process that assembles the mount.
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", fmt.Errorf("mount: create %s: %w", runDir, err)
	}
	dir, err := os.MkdirTemp(runDir, "mount-*")
	if err != nil {
		return "", fmt.Errorf("mount: create scratch dir: %w", err)
	}
	return dir, nil
}

// overlayOptions builds the overlay option string that carries the trust
// posture: the EROFS metadata as the top lower layer, basedir as the data-only
// lower ("::") its redirects resolve through, and fs-verity required on every
// redirected object. With noVerity the verity option is left out, so overlayfs
// does not validate the objects, which carry no fs-verity.
func overlayOptions(scratch, basedir string, noVerity bool) string {
	opts := "lowerdir=" + scratch + "::" + basedir + ",redirect_dir=on,metacopy=on"
	if noVerity {
		return opts
	}
	return opts + ",verity=require"
}

// validateSpec checks a composefs mount spec before any I/O: no empty fields,
// a well-formed sha256 fs-verity digest, and a basedir free of the delimiter
// bytes that would split the overlay option string (',' separates options and
// ':' separates lower layers; the scratch path is ocicfs-generated under
// runDir, so a violation there would be a bug, not an input).
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
	if _, err := digestutil.ParseSHA256(s.FsverityDigest); err != nil {
		return fmt.Errorf("mount: fs-verity digest: %w", err)
	}
	if strings.ContainsAny(s.Basedir, ":,") {
		return errors.New("mount: basedir cannot contain ':' or ','")
	}
	return nil
}

// warnClose closes c, logging a failure rather than dropping it.
func warnClose(msg string, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn(msg, "err", err)
	}
}

// removeDir removes dir, logging a failure rather than dropping it.
func removeDir(dir string) {
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("mount: failed to remove scratch dir", "dir", dir, "err", err)
	}
}
