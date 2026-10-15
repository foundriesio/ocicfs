// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package digestutil validates and parses sha256 digests, the only digest
// algorithm ocicfs's content-addressed store uses.
//
// A digest is validated exactly where it is not yet trustworthy: where outside
// bytes become a digest (a parsed reference, decoded JSON) and where a digest
// becomes a filesystem path (the store path constructors). A typed digest
// between those points has already passed one of them and is not re-validated.
package digestutil

import (
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

// ParseSHA256 parses s and returns it only if it is a valid sha256 digest.
func ParseSHA256(s string) (digest.Digest, error) {
	d, err := digest.Parse(s)
	if err != nil {
		return "", err
	}
	if err := ValidateSHA256(d); err != nil {
		return "", err
	}
	return d, nil
}

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
