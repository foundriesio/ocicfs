// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/refstore"
)

// List returns the images present in the local store: every referenced
// image, then every image a hold alone keeps.
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
	referenced := make(map[digest.Digest]struct{}, len(records))
	for _, rec := range records {
		size, err := bindingSize(e.img, rec)
		if err != nil {
			return nil, err
		}
		referenced[rec.Identity.Digest] = struct{}{}
		images = append(images, Image{
			Ref:    rec.Ref,
			Digest: rec.Identity.Digest.String(),
			Size:   size,
			Holder: rec.Holder,
		})
	}

	// An image no reference names is still present while a hold keeps it —
	// a runtime that pulled it under WithHold owns it that way — so it is
	// listed too, by digest alone, after the referenced images. held is
	// sorted. A held digest with no binding is a loose object or a broken
	// store, neither an image; gc is where the latter is reported.
	held, err := e.holds.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, root := range held {
		if _, ok := referenced[root]; ok {
			continue
		}
		platforms, err := e.img.Platforms(ctx, root)
		if err != nil {
			slog.Warn("ocicfs: skipping held image with unreadable bindings", "digest", root, "err", err)
			continue
		}
		if len(platforms) == 0 {
			continue
		}
		images = append(images, Image{
			Digest: root.String(),
			Size:   bindingObjectSize(e.img, root, platforms[0], "held image "+root.String()),
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
	return bindingObjectSize(imgStore, rec.Identity.Digest, platforms[0], "ref "+rec.Ref), nil
}

// bindingObjectSize reports the size of the composefs image object the
// binding for (root, platform) records, or 0 with a warning naming what
// (a ref or a held image) when the binding is missing or unreadable. The
// binding is an auxiliary, regenerable index: a bad one must not brick the
// whole listing (matching refstore.List's treatment of bad records); a
// re-pull restores it.
func bindingObjectSize(imgStore *imagestore.Store, root digest.Digest, platform ocispec.Platform, what string) int64 {
	b, err := imgStore.Get(root, platform)
	if err != nil {
		slog.Warn("ocicfs: no usable image binding for listed image", "image", what, "err", err)
		return 0
	}
	return b.Descriptor.Size
}

func (e *engine) remove(ctx context.Context, ref string, opts ...RemoveOption) error {
	var cfg RemoveOptions
	for _, opt := range opts {
		opt(&cfg)
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

	// Hold the identity digest's lock across the holder check and the delete, so
	// a concurrent hold cannot slip a holder in after the check and be stranded
	// by the delete. Every hold is keyed by the identity, so this one lock and
	// this one holder directory cover the image however it was named. Without
	// --force a present holder declines the removal by name; with --force the
	// holds are cleared first.
	root := rec.Identity.Digest
	unlock, err := e.holds.Lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	// This is the single cancellation gate, checked under the lock and before any
	// mutation: the lock wait is not interruptible, and everything past this point
	// — clearing holds and deleting the ref and bindings — runs atomically with
	// respect to cancellation, so a forced remove never clears holds and then
	// bails partway through.
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Force {
		if err := e.holds.Clear(root); err != nil {
			return err
		}
	} else {
		holders, err := e.holds.Holders(root)
		if err != nil {
			return err
		}
		if len(holders) > 0 {
			// Name the digest the holders sit under as well: releasing one takes
			// that digest, and it is not always the one the caller typed.
			return fmt.Errorf("image is held by %s under %s; release the hold or use --force", strings.Join(holders, ", "), root)
		}
	}

	platforms, err := parseRecordPlatforms(rec.Platforms)
	if err != nil {
		return err
	}
	records, err := e.refs.List()
	if err != nil {
		return err
	}
	toDelete, err := unsharedBindings(rec.Ref, rec.Identity.Digest, platforms, records)
	if err != nil {
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
		if err := e.img.Delete(rec.Identity.Digest, platform); err != nil {
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

// unsharedBindings returns the platforms whose binding for the identity digest
// root no reference other than ref still resolves to.
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

// bindingUsedByOther reports whether a reference other than ref resolves to the
// same binding. The comparison is between identities, the digest a binding is
// keyed by: comparing the resolved descriptors instead would let a removed tag
// delete the binding an index-pinned reference for the same image still needs.
func bindingUsedByOther(ref string, root digest.Digest, platform ocispec.Platform, records []refstore.Record) (bool, error) {
	for _, other := range records {
		if other.Ref == ref || other.Identity.Digest != root {
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
// for the same identity always carry the identical config-derived platform
// string, so the two equivalences cannot diverge here.
func samePlatform(a, b ocispec.Platform) bool {
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant
}
