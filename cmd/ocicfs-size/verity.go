// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

// verityDigestSize is the size of a sha256 Merkle-tree digest.
const verityDigestSize = 32

// alloc returns the bytes s occupies when allocated in blocks of b bytes.
func alloc(s, b int64) int64 {
	return (s + b - 1) / b * b
}

// verityBytes returns the host blocks fs-verity adds to an ext4 file of s
// bytes sealed with Merkle blocks of vb bytes: the tree's blocks plus one
// for the descriptor. The tree starts at the 64 KiB boundary past the end
// of the data and the gap is a hole, so only the tree and the descriptor
// are allocated.
func verityBytes(s, vb, hostBlock int64) int64 {
	arity := vb / verityDigestSize
	n := (s + vb - 1) / vb
	var tree int64
	for n > 1 {
		n = (n + arity - 1) / arity
		tree += n
	}
	return alloc(tree*vb, hostBlock) + hostBlock
}
