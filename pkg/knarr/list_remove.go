// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/refstore"
)

// List returns the images present in the local store.
func (e *engine) List(ctx context.Context) ([]Image, error) {
	images, err := e.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	return images, nil
}

// Remove forgets a local image.
func (e *engine) Remove(ctx context.Context, ref string, opts ...RemoveOption) error {
	if err := e.remove(ctx, ref, opts...); err != nil {
		return fmt.Errorf("remove %s: %w", ref, err)
	}
	return nil
}

func (e *engine) list(ctx context.Context) ([]Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := e.refs.List()
	if err != nil {
		return nil, err
	}
	// records is already sorted by ref.
	images := make([]Image, 0, len(records))
	for _, rec := range records {
		size, err := bindingSize(e.img, rec)
		if err != nil {
			return nil, err
		}
		images = append(images, Image{
			Ref:    rec.Ref,
			Digest: rec.Resolved.Digest.String(),
			Size:   size,
		})
	}
	return images, nil
}

// bindingSize reports the size of the composefs image object recorded for the
// record's first platform, keeping Image.Size consistent between Pull and List.
func bindingSize(imgStore *imagestore.Store, rec refstore.Record) (int64, error) {
	platforms, err := parseRecordPlatforms(rec.Platforms)
	if err != nil {
		return 0, err
	}
	if len(platforms) == 0 {
		return 0, fmt.Errorf("ref %s records no platforms", rec.Ref)
	}
	b, err := imgStore.Get(rec.Resolved.Digest, platforms[0])
	if err != nil {
		// The binding is an auxiliary, regenerable index: a missing or invalid
		// one must not brick the whole listing (matching refstore.List's
		// treatment of bad records), so list the image with a zero size and
		// warn; a re-pull restores the binding.
		slog.Warn("knarr: no usable image binding for listed ref", "ref", rec.Ref, "err", err)
		return 0, nil
	}
	return b.Descriptor.Size, nil
}

func (e *engine) remove(ctx context.Context, ref string, opts ...RemoveOption) error {
	var cfg RemoveOptions
	for _, opt := range opts {
		opt(&cfg)
	}
	_ = cfg.Force

	if err := ctx.Err(); err != nil {
		return err
	}
	normalized, err := normalizeRef(ref)
	if err != nil {
		return err
	}
	rec, err := e.refs.Get(normalized)
	if errors.Is(err, refstore.ErrNotFound) {
		return fmt.Errorf("no such image: %s: %w", ref, errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return err
	}
	platforms, err := parseRecordPlatforms(rec.Platforms)
	if err != nil {
		return err
	}
	records, err := e.refs.List()
	if err != nil {
		return err
	}
	toDelete, err := unsharedBindings(rec.Ref, rec.Resolved.Digest, platforms, records)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Delete the ref before its bindings, the reverse of the write order (a pull
	// writes the binding then the ref). This keeps "a surviving ref always
	// resolves to a present binding" across a crash: a crash after the ref is
	// gone leaves at most an orphan binding, which is auxiliary and reclaimed by
	// gc, never a live ref pointing at a missing binding.
	if err := e.refs.Delete(normalized); err != nil {
		return err
	}
	for _, platform := range toDelete {
		if err := e.img.Delete(rec.Resolved.Digest, platform); err != nil {
			return fmt.Errorf("delete image binding: %w", err)
		}
	}
	return nil
}

func parseRecordPlatforms(values []string) ([]ocispec.Platform, error) {
	platforms := make([]ocispec.Platform, 0, len(values))
	for _, value := range values {
		platform, err := ocilayout.ParsePlatform(value)
		if err != nil {
			return nil, fmt.Errorf("parse platform %q: %w", value, err)
		}
		platforms = append(platforms, platform)
	}
	return platforms, nil
}

func unsharedBindings(ref string, root digest.Digest, platforms []ocispec.Platform, records []refstore.Record) ([]ocispec.Platform, error) {
	var out []ocispec.Platform
	for _, platform := range platforms {
		shared, err := bindingUsedByOther(ref, root, platform, records)
		if err != nil {
			return nil, err
		}
		if !shared {
			out = append(out, platform)
		}
	}
	return out, nil
}

func bindingUsedByOther(ref string, root digest.Digest, platform ocispec.Platform, records []refstore.Record) (bool, error) {
	for _, other := range records {
		if other.Ref == ref || other.Resolved.Digest != root {
			continue
		}
		for _, value := range other.Platforms {
			otherPlatform, err := ocilayout.ParsePlatform(value)
			if err != nil {
				return false, fmt.Errorf("parse platform %q for %s: %w", value, other.Ref, err)
			}
			if samePlatform(otherPlatform, platform) {
				return true, nil
			}
		}
	}
	return false, nil
}

// samePlatform compares fields literally, without the default-variant
// canonicalization the binding filename applies (arm64/v8 == arm64): records
// for the same root always carry the identical config-derived platform string,
// so the two equivalences cannot diverge here.
func samePlatform(a, b ocispec.Platform) bool {
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant
}
