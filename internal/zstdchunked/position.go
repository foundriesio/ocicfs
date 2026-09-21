// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package zstdchunked parses and reconstructs files from zstd:chunked layers.
package zstdchunked

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/opencontainers/go-digest"
)

const (
	// ManifestChecksumKey identifies the compressed TOC digest annotation.
	ManifestChecksumKey = "io.github.containers.zstd-chunked.manifest-checksum"
	// ManifestPositionKey identifies the TOC position annotation.
	ManifestPositionKey = "io.github.containers.zstd-chunked.manifest-position"

	// maxTOCBytes caps both the compressed and the uncompressed TOC.
	maxTOCBytes = 64 << 20
	// frameHeaderSize is the zstd skippable-frame header (magic plus
	// little-endian length) that ends at Pos.Offset, where the compressed TOC begins.
	frameHeaderSize = 8
)

// Pos locates and authenticates a compressed TOC in a layer blob.
type Pos struct {
	Offset          int64
	CompressedLen   int64
	UncompressedLen int64
	Checksum        digest.Digest
}

// Position parses the zstd:chunked TOC annotations.
func Position(annotations map[string]string, blobSize int64) (Pos, bool, error) {
	position, hasPosition := annotations[ManifestPositionKey]
	checksum, hasChecksum := annotations[ManifestChecksumKey]
	if !hasPosition && !hasChecksum {
		return Pos{}, false, nil
	}
	if !hasPosition || !hasChecksum {
		return Pos{}, false, fmt.Errorf("zstdchunked: incomplete manifest annotations")
	}
	if blobSize < 0 {
		return Pos{}, false, fmt.Errorf("zstdchunked: negative blob size %d", blobSize)
	}

	fields := strings.Split(position, ":")
	if len(fields) != 4 {
		return Pos{}, false, fmt.Errorf("zstdchunked: malformed manifest position %q", position)
	}
	values := make([]int64, len(fields))
	for i, field := range fields {
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return Pos{}, false, fmt.Errorf("zstdchunked: malformed manifest position field %q: %w", field, err)
		}
		if value < 0 {
			return Pos{}, false, fmt.Errorf("zstdchunked: negative manifest position field %d", value)
		}
		values[i] = value
	}
	if values[3] != 1 {
		return Pos{}, false, fmt.Errorf("zstdchunked: unsupported manifest type %d", values[3])
	}
	if values[0] < frameHeaderSize {
		return Pos{}, false, fmt.Errorf("zstdchunked: manifest offset %d precedes its frame header", values[0])
	}
	if values[1] > maxTOCBytes || values[2] > maxTOCBytes {
		return Pos{}, false, fmt.Errorf("zstdchunked: manifest exceeds %d-byte limit", maxTOCBytes)
	}
	if values[0] > blobSize || values[1] > blobSize-values[0] {
		return Pos{}, false, fmt.Errorf("zstdchunked: manifest byte range exceeds %d-byte blob", blobSize)
	}

	d := digest.Digest(checksum)
	if err := digestutil.ValidateSHA256(d); err != nil {
		return Pos{}, false, fmt.Errorf("zstdchunked: invalid manifest checksum: %w", err)
	}
	return Pos{
		Offset:          values[0],
		CompressedLen:   values[1],
		UncompressedLen: values[2],
		Checksum:        d,
	}, true, nil
}
