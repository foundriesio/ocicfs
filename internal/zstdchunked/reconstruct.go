// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package zstdchunked

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Reconstruct returns a reader of the content of one regular file from a
// zstd:chunked layer. The caller must close it; closing it does not close
// compressed, which stays the caller's.
//
// compressed must supply the bytes the layer blob holds from e.Offset to
// e.EndOffset. The reader yields exactly e.Size bytes: when the content turns
// out shorter or longer, Read fails instead of returning io.EOF. The length is
// therefore only checked for a caller that reads until io.EOF, as io.Copy does.
//
// The reader does not compare the content with e.Digest: the caller must hash
// what it reads and check it, and must discard what it read when Read fails.
func Reconstruct(e Entry, compressed io.Reader) (io.ReadCloser, error) {
	if e.Size < 0 {
		return nil, fmt.Errorf("zstdchunked: negative file size %d", e.Size)
	}
	decoder, err := zstd.NewReader(
		compressed,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(decoderMaxMemory),
		// Keep a bytes.Reader input off the in-memory DecodeAll path.
		zstd.WithDecodeBuffersBelow(0),
	)
	if err != nil {
		return nil, fmt.Errorf("zstdchunked: create file decoder: %w", err)
	}
	return &fileReader{decoder: decoder, size: e.Size, remaining: e.Size}, nil
}

// fileReader reads a decompressed file and enforces its size.
type fileReader struct {
	decoder   *zstd.Decoder
	size      int64
	remaining int64
}

func (f *fileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.remaining == 0 {
		return 0, f.finish()
	}
	if int64(len(p)) > f.remaining {
		p = p[:f.remaining]
	}
	n, err := f.decoder.Read(p)
	f.remaining -= int64(n)
	if errors.Is(err, io.EOF) && f.remaining > 0 {
		return n, fmt.Errorf("zstdchunked: file ends %d bytes short of %d: %w", f.remaining, f.size, io.ErrUnexpectedEOF)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("zstdchunked: reconstruct file: %w", err)
	}
	return n, err
}

// finish reports io.EOF when the decompressed stream ends at the file size.
func (f *fileReader) finish() error {
	var extra [1]byte
	n, err := f.decoder.Read(extra[:])
	if n != 0 {
		return fmt.Errorf("zstdchunked: reconstructed file exceeds %d bytes", f.size)
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if err == nil {
		return fmt.Errorf("zstdchunked: reconstructed file did not end at %d bytes", f.size)
	}
	return fmt.Errorf("zstdchunked: finish reconstructing file: %w", err)
}

func (f *fileReader) Close() error {
	f.decoder.Close()
	return nil
}
