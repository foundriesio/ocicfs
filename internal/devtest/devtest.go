// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package devtest provides fixtures for knarr's privileged end-to-end tests: a
// verity-capable store root. fs-verity needs a real backing filesystem, which
// tmpfs and overlayfs cannot provide, so the dev container mounts a loop-backed
// ext4 filesystem with the fs-verity feature and the helpers here root the store
// on it. It is imported only from end-to-end test files.
package devtest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrNoVerity reports that the dev container's verity-capable filesystem is not
// mounted — the test is running outside the privileged dev container. A
// verity-dependent test should skip when it sees this.
var ErrNoVerity = errors.New("devtest: verity-capable test root unavailable")

// verityDir is the knarr default data root, where the privileged dev container
// mounts a loop-backed ext4 filesystem with the fs-verity feature. Tests root an
// isolated store in a fresh subdirectory of it.
const verityDir = "/var/lib/knarr"

// VerityRoot returns a fresh empty directory on the dev container's
// verity-capable filesystem for use as a knarr store root, along with a cleanup
// function to remove it. It returns ErrNoVerity when that filesystem is not
// mounted or is not usable by this process (for example a root-owned default
// store left over from a host run), so the caller skips.
func VerityRoot() (dir string, cleanup func(), err error) {
	if fi, statErr := os.Stat(verityDir); statErr != nil || !fi.IsDir() {
		return "", nil, ErrNoVerity
	}
	// The entrypoint mkdir's verityDir before it tries to mount the loop-backed
	// ext4 filesystem, so the directory existing is not proof the fixture is
	// there. Require it to be a distinct mount; otherwise the verity-capable
	// filesystem was not set up (unprivileged run) and the caller should skip
	// rather than fall through onto a plain directory that cannot enable verity.
	if !isMountpoint(verityDir) {
		return "", nil, ErrNoVerity
	}
	d, err := os.MkdirTemp(verityDir, "e2e-*")
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrNoVerity, err)
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}

// isMountpoint reports whether dir is the root of a mounted filesystem, detected
// by it residing on a different device than its parent directory.
func isMountpoint(dir string) bool {
	var st, parent unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return false
	}
	if err := unix.Stat(filepath.Dir(dir), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// IsVerityUnsupported reports whether err indicates the underlying filesystem
// does not support fs-verity, anywhere in its chain, so a verity-dependent test
// can skip rather than fail. It matches the errnos the enable/measure ioctls
// return on such a filesystem (for example tmpfs).
func IsVerityUnsupported(err error) bool {
	return err != nil && (errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOTTY) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS))
}
