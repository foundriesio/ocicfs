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
	"time"
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

// enableRetryDelays are the waits between attempts when the enable ioctl
// reports ETXTBSY. They total about one second, far longer than a forked child
// takes to reach exec.
var enableRetryDelays = []time.Duration{
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond,
	10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond,
	100 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond,
}

// Enable turns on fs-verity (sha256) for the open file. The file must be opened
// read-only with no concurrent writable handle. After this returns the file is
// immutable and the kernel verifies its contents on read.
//
// A child process forked by another goroutine holds a copy of every open
// descriptor until it calls exec, including a writable one this process has
// already closed, and the kernel refuses to enable fs-verity with ETXTBSY
// meanwhile. Enable retries ETXTBSY for about a second to ride out that window.
func Enable(f *os.File) error {
	arg := unix.FsverityEnableArg{
		Version:        1,
		Hash_algorithm: unix.FS_VERITY_HASH_ALG_SHA256,
		Block_size:     BlockSize,
	}
	for i := 0; ; i++ {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(unix.FS_IOC_ENABLE_VERITY), uintptr(unsafe.Pointer(&arg))) //nolint:gosec // fs-verity enable ioctl passes the kernel arg by pointer
		if errno == 0 {
			return nil
		}
		if errno != unix.ETXTBSY || i == len(enableRetryDelays) {
			return fmt.Errorf("verity: enable %s: %w", f.Name(), errno)
		}
		time.Sleep(enableRetryDelays[i])
	}
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
