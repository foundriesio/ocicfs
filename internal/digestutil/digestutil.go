// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package digestutil validates sha256 digests, the only digest algorithm
// knarr's content-addressed store uses.
package digestutil

import (
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

// ValidateSHA256 rejects a digest that is malformed or whose algorithm is not
// sha256. Checking the algorithm before Validate keeps the "non-sha256" verdict
// even for an algorithm this binary does not link.
func ValidateSHA256(d digest.Digest) error {
	s := string(d)
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return fmt.Errorf("malformed digest %q", s)
	}
	if digest.Algorithm(s[:i]) != digest.SHA256 {
		return fmt.Errorf("non-sha256 digest algorithm %s", s[:i])
	}
	if err := d.Validate(); err != nil {
		return fmt.Errorf("invalid digest %q: %w", s, err)
	}
	return nil
}
