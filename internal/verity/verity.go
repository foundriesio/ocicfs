// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package verity wraps the fs-verity enable and measure ioctls used to make a
// content object immutable and kernel-verified on read.
package verity

import (
	"encoding/hex"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sha256Size is the byte length of a sha256 fs-verity digest.
const sha256Size = 32

// BlockSize is the fs-verity Merkle-tree block size. It is pinned at 4096 so the
// kernel-measured per-file digest of a given object is identical across hosts
// regardless of page size, which keeps the on-disk format stable. A kernel that
// cannot build a 4096-byte Merkle block on the host page size rejects Enable
// with EINVAL.
const BlockSize = 4096

// Enable turns on fs-verity (sha256) for the open file. The file must be opened
// read-only with no concurrent writable handle. After this returns the file is
// immutable and the kernel verifies its contents on read.
func Enable(f *os.File) error {
	arg := unix.FsverityEnableArg{
		Version:        1,
		Hash_algorithm: unix.FS_VERITY_HASH_ALG_SHA256,
		Block_size:     BlockSize,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(unix.FS_IOC_ENABLE_VERITY), uintptr(unsafe.Pointer(&arg))) //nolint:gosec // fs-verity enable ioctl passes the kernel arg by pointer
	if errno != 0 {
		return fmt.Errorf("verity: enable %s: %w", f.Name(), errno)
	}
	return nil
}

// Measure returns the file's fs-verity digest formatted as "sha256:<hex>". It
// fails if fs-verity is not enabled on the file.
func Measure(f *os.File) (string, error) {
	// fsverity_digest is a 4-byte header (algorithm, size) followed by the
	// digest bytes. Allocate the header plus room for a sha256 digest and tell
	// the kernel how much space is available in the size field.
	const headerSize = int(unsafe.Sizeof(unix.FsverityDigest{}))
	buf := make([]byte, headerSize+sha256Size)
	d := (*unix.FsverityDigest)(unsafe.Pointer(&buf[0])) //nolint:gosec // overlay the fixed-layout fsverity_digest header to set the size field
	d.Size = sha256Size
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(unix.FS_IOC_MEASURE_VERITY), uintptr(unsafe.Pointer(&buf[0]))) //nolint:gosec // measure ioctl writes the digest into the buffer by pointer
	if errno != 0 {
		return "", fmt.Errorf("verity: measure %s: %w", f.Name(), errno)
	}
	if d.Algorithm != unix.FS_VERITY_HASH_ALG_SHA256 {
		return "", fmt.Errorf("verity: %s: unexpected hash algorithm %d", f.Name(), d.Algorithm)
	}
	if int(d.Size) != sha256Size {
		return "", fmt.Errorf("verity: %s: unexpected digest size %d", f.Name(), d.Size)
	}
	return "sha256:" + hex.EncodeToString(buf[headerSize:headerSize+sha256Size]), nil
}
