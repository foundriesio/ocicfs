// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociseal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
)

// MergedAnnotationPrefix is the namespace every merged-EROFS sealing
// annotation shares, regardless of the digest algorithm or suffix it records.
const MergedAnnotationPrefix = "composefs.merged.erofs.v1."

// VerifyAnnotations verifies any sealing annotations in res against the
// materialized merged image and stored config blob.
//
// A canonical construction reproduces the sealed digest, so a present merged
// or config annotation must equal the measured value; a mismatch fails closed
// and a missing annotation is accepted as an unsealed pull.
//
// A non-canonical construction cannot reproduce the value a standard sealing
// annotation claims, so any annotation under the merged-EROFS namespace fails
// closed regardless of its suffix or value. What it does reproduce is its own
// deterministic fallback digest, so a present knarr fallback annotation must
// equal the measured value, with the same fail-closed mismatch handling. A
// non-canonical image with neither is a valid unsealed pull.
func VerifyAnnotations(ctx context.Context, store *objectstore.Store, res ocichain.Resolved, mergedFsverity string, canonical bool) error {
	if store == nil {
		return errors.New("ociseal: nil object store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	mergedKey, configKey := MergedAnnotation, ConfigAnnotation
	if !canonical {
		for key := range res.Manifest.Annotations {
			if strings.HasPrefix(key, MergedAnnotationPrefix) {
				return fmt.Errorf("ociseal: sealing annotation %s on non-canonical construction", scrubControl(key))
			}
		}
		mergedKey, configKey = FallbackMergedAnnotation, FallbackConfigAnnotation
	}
	if value, ok := res.Manifest.Annotations[mergedKey]; ok {
		if err := verifyAnnotation(mergedKey, value, mergedFsverity); err != nil {
			return err
		}
	}
	if value, ok := res.Manifest.Config.Annotations[configKey]; ok {
		measured, err := store.VerityDigest(res.ConfigDesc.Digest)
		if err != nil {
			return fmt.Errorf("ociseal: measure stored config: %w", err)
		}
		if err := verifyAnnotation(configKey, value, measured); err != nil {
			return err
		}
	}
	return nil
}

func verifyAnnotation(key, value, actual string) error {
	expected, err := ParseAnnotation(value)
	if err != nil {
		return fmt.Errorf("ociseal: annotation %s: %w", key, err)
	}
	if expected != actual {
		return fmt.Errorf("ociseal: annotation %s mismatch: expected %s, got %s", key, expected, actual)
	}
	return nil
}
