// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package verity

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const (
	hashesPerBlock = BlockSize / sha256.Size
	descriptorSize = 256
)

// Digest streams r and returns its fs-verity file digest using sha256,
// 4096-byte blocks, and no salt. The result is formatted as "sha256:<hex>".
func Digest(ctx context.Context, r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("verity: nil digest reader")
	}

	var (
		tree merkleTree
		size uint64
		buf  [BlockSize]byte
	)
	for {
		n, err := io.ReadFull(&contextReader{ctx: ctx, r: r}, buf[:])
		if n > 0 {
			if size > ^uint64(0)-uint64(n) {
				return "", errors.New("verity: file size overflows uint64")
			}
			size += uint64(n)
			clear(buf[n:])
			tree.add(0, sha256.Sum256(buf[:]))
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			root := tree.root()
			return descriptorDigest(size, root), nil
		default:
			return "", fmt.Errorf("verity: read file: %w", err)
		}
	}
}

type (
	merkleTree struct {
		levels [][][sha256.Size]byte
		count  uint64
	}

	contextReader struct {
		ctx context.Context
		r   io.Reader
	}
)

func (t *merkleTree) add(level int, sum [sha256.Size]byte) {
	for len(t.levels) <= level {
		t.levels = append(t.levels, nil)
	}
	t.levels[level] = append(t.levels[level], sum)
	if level == 0 {
		t.count++
	}
	if len(t.levels[level]) == hashesPerBlock {
		parent := hashBlock(t.levels[level])
		t.levels[level] = nil
		t.add(level+1, parent)
	}
}

func (t *merkleTree) root() [sha256.Size]byte {
	if t.count == 0 {
		return [sha256.Size]byte{}
	}
	for level := 0; ; level++ {
		if len(t.levels[level]) == 1 && noHashesAbove(t.levels, level) {
			return t.levels[level][0]
		}
		if len(t.levels[level]) > 0 {
			parent := hashBlock(t.levels[level])
			t.levels[level] = nil
			t.add(level+1, parent)
		}
	}
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func hashBlock(sums [][sha256.Size]byte) [sha256.Size]byte {
	var block [BlockSize]byte
	for i := range sums {
		copy(block[i*sha256.Size:], sums[i][:])
	}
	return sha256.Sum256(block[:])
}

func noHashesAbove(levels [][][sha256.Size]byte, level int) bool {
	for i := level + 1; i < len(levels); i++ {
		if len(levels[i]) > 0 {
			return false
		}
	}
	return true
}

// descriptorDigest hashes the fsverity_descriptor for a file of the given size
// and Merkle root; the descriptor's salt, signature, and reserved fields stay
// zero.
func descriptorDigest(size uint64, root [sha256.Size]byte) string {
	var descriptor [descriptorSize]byte
	descriptor[0] = 1                                     // version
	descriptor[1] = 1                                     // hash_algorithm: sha256
	descriptor[2] = 12                                    // log_blocksize: 4096-byte blocks
	binary.LittleEndian.PutUint64(descriptor[8:16], size) // data_size
	copy(descriptor[16:16+sha256.Size], root[:])          // root_hash
	sum := sha256.Sum256(descriptor[:])
	return "sha256:" + hex.EncodeToString(sum[:])
}
