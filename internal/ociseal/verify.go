// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociseal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/ocichain"
)

// MergedAnnotationPrefix is the namespace every merged-EROFS sealing
// annotation shares, regardless of the digest algorithm or suffix it records.
const MergedAnnotationPrefix = "composefs.merged.erofs.v1."

// VerifyAnnotations verifies any sealing annotations in res against the
// materialized merged image and stored config blob.
//
// The merged annotation to compare is the one SelectMergedAnnotation picks,
// by presence and never by the canonical flag: the flag is computed from the
// very content the annotation vouches for, so letting it choose the key would
// let that content decide which seal it is held to. A selected annotation
// must equal the measured value; a mismatch fails closed and a manifest
// carrying none is accepted as an unsealed pull.
//
// A non-canonical construction cannot reproduce the value a standard sealing
// annotation claims, so any annotation under the merged-EROFS namespace fails
// closed regardless of its suffix or value; what it does reproduce is its own
// deterministic fallback digest, which the ocicfs fallback annotation records.
//
// The config annotation is keyed by the canonical flag: the standard key on a
// canonical construction, the ocicfs fallback key otherwise, with the same
// fail-closed mismatch handling.
func VerifyAnnotations(ctx context.Context, store *objectstore.Store, res ocichain.Resolved, mergedFsverity string, canonical bool) error {
	if store == nil {
		return errors.New("ociseal: nil object store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	configKey := ConfigAnnotation
	if !canonical {
		for key := range res.Manifest.Annotations {
			if strings.HasPrefix(key, MergedAnnotationPrefix) {
				return fmt.Errorf("ociseal: sealing annotation %s on non-canonical construction", scrubControl(key))
			}
		}
		configKey = FallbackConfigAnnotation
	}
	mergedKey, expected, err := SelectMergedAnnotation(res.Manifest.Annotations)
	if err != nil {
		return err
	}
	if mergedKey != "" && expected != mergedFsverity {
		return fmt.Errorf("ociseal: annotation %s mismatch: expected %s, got %s", mergedKey, expected, mergedFsverity)
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

// SelectMergedAnnotation selects the merged-image sealing annotation a
// manifest is held to and returns its key and the fs-verity digest it
// records, or an empty key when the manifest carries none. Selection is by
// annotation presence alone. The standard key wins over the ocicfs fallback
// key when both are present, and the fallback's value is then not read. A
// malformed value and a merged-image annotation of an unrecognized digest
// kind both fail closed: the publisher sealed the image, so treating it as
// unsealed would silently drop the seal. An unrecognized digest kind
// coexisting with a recognized key does not fail: every annotation sits
// inside the hash-pinned manifest, so the combination is publisher-authored —
// dual-sealing for a format migration — not a downgrade an attacker can
// induce, and the recognized seal pins the content at full strength on its
// own.
func SelectMergedAnnotation(annotations map[string]string) (key, fsverity string, err error) {
	for _, key := range []string{MergedAnnotation, FallbackMergedAnnotation} {
		value, ok := annotations[key]
		if !ok {
			continue
		}
		sealed, err := ParseAnnotation(value)
		if err != nil {
			return "", "", fmt.Errorf("ociseal: sealing annotation %s: %w", key, err)
		}
		return key, sealed, nil
	}
	for key := range annotations {
		if strings.HasPrefix(key, MergedAnnotationPrefix) {
			return "", "", fmt.Errorf("ociseal: unverifiable sealing annotation %s: unrecognized digest kind", scrubControl(key))
		}
	}
	return "", "", nil
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
