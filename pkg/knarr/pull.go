// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/materialize"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/ociremote"
	"github.com/foundriesio/knarr/internal/refstore"
)

// Pull fetches ref into the local store and returns the stored image.
func (e *engine) Pull(ctx context.Context, ref string, opts ...PullOption) (*Image, error) {
	img, err := e.pull(ctx, ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	return img, nil
}

func (e *engine) pull(ctx context.Context, ref string, opts ...PullOption) (*Image, error) {
	var cfg PullOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	var (
		src  ocichain.BlobSource
		root digest.Digest
		err  error
	)
	if cfg.FromDir == "" {
		src, root, err = ociremote.Open(ctx, ref)
		if err != nil {
			return nil, err
		}
	} else {
		root, err = parseDigestPinnedRef(ref)
		if err != nil {
			return nil, err
		}
		src, err = ocilayout.Open(cfg.FromDir)
		if err != nil {
			return nil, err
		}
	}

	platform := hostPlatform()
	if cached, ok, err := verifiedPulledImage(ctx, e.obj, e.img, root, platform); err != nil {
		return nil, err
	} else if ok {
		normalized, err := normalizeRef(ref)
		if err != nil {
			return nil, err
		}
		if err := e.refs.Put(normalized, cached.rootDesc, time.Now(), []string{ocilayout.PlatformString(&cached.platform)}); err != nil {
			return nil, err
		}
		return &Image{
			Ref:    ref,
			Digest: root.String(),
			Size:   cached.descriptor.Size,
		}, nil
	}

	built, err := materialize.Materialize(ctx, e.obj, e.img, src, root, platform)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeRef(ref)
	if err != nil {
		return nil, err
	}
	if err := e.refs.Put(normalized, built.RootDesc, time.Now(), []string{ocilayout.PlatformString(&built.Platform)}); err != nil {
		return nil, err
	}

	return &Image{
		Ref:    ref,
		Digest: root.String(),
		Size:   built.Descriptor.Size,
	}, nil
}

type cachedImage struct {
	rootDesc   ocispec.Descriptor
	platform   ocispec.Platform
	descriptor ocispec.Descriptor
}

// verifiedPulledImage reports whether (root, platform) is already materialized
// and trustworthy: a binding exists, the bound composefs object measures to the
// recorded fs-verity digest, and the metadata chain resolves from the local
// store. Any failure short of a canceled context reports "not cached" rather
// than an error, so the caller falls through to a full pull, which rewrites the
// same content idempotently and heals whatever was stale or missing.
func verifiedPulledImage(ctx context.Context, obj *objectstore.Store, imgStore *imagestore.Store, root digest.Digest, platform ocispec.Platform) (cachedImage, bool, error) {
	if err := ctx.Err(); err != nil {
		return cachedImage{}, false, err
	}
	b, err := imgStore.Get(root, platform)
	if err != nil {
		return cachedImage{}, false, nil
	}
	if err := obj.CheckVerity(); err != nil {
		return cachedImage{}, false, nil
	}
	measured, err := obj.VerityDigest(b.Descriptor.Digest)
	if err != nil || measured != b.FsverityDigest {
		return cachedImage{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return cachedImage{}, false, err
	}
	res, err := ocichain.Resolve(ctx, casBlobSource{store: obj}, root, platform)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return cachedImage{}, false, err
		}
		return cachedImage{}, false, nil
	}
	return cachedImage{rootDesc: res.RootDesc, platform: res.Platform, descriptor: b.Descriptor}, true, nil
}

// resolveLocalRef returns the root digest ref names. A digest-pinned ref
// carries the digest itself; any other ref string is normalized and resolves
// through the local reference store to the digest its most recent pull
// recorded under that normalized form, so a pulled tag works with the verbs
// that read only local state regardless of which equivalent alias named it.
// Resolution never touches the network; the returned digest, not the ref, is
// what every subsequent lookup and verification keys on. A ref that reads as a
// digest-pin attempt (it contains "@" or is a bare digest) reports its parse
// error instead of falling through, because pull can never have recorded such
// a string.
func (e *engine) resolveLocalRef(ref string) (digest.Digest, error) {
	root, perr := parseDigestPinnedRef(ref)
	if perr == nil {
		return root, nil
	}
	if strings.ContainsRune(ref, '@') || looksLikeBareDigest(ref) {
		return "", perr
	}
	normalized, err := normalizeRef(ref)
	if err != nil {
		return "", err
	}
	rec, err := e.refs.Get(normalized)
	if errors.Is(err, refstore.ErrNotFound) {
		return "", fmt.Errorf("not digest-pinned and not recorded by a pull; run `knarr pull` first or pin a digest (name@sha256:…): %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return "", err
	}
	return rec.Resolved.Digest, nil
}

// normalizeRef canonicalizes ref for use as the refstore lookup key, so a ref
// stored by one caller and looked up under a differently-formed but
// equivalent ref (a short name, a missing tag, a non-Docker-Hub host, or
// name@digest) resolves to the same record. A bare digest is already
// canonical and passes through unchanged; everything else is canonicalized
// through the OCI reference grammar, which folds a short name like "ubuntu"
// to its fully qualified form (e.g. "index.docker.io/library/ubuntu:latest").
func normalizeRef(ref string) (string, error) {
	if looksLikeBareDigest(ref) {
		return ref, nil
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("normalize ref %s: %w", ref, err)
	}
	return r.Name(), nil
}

func parseDigestPinnedRef(ref string) (digest.Digest, error) {
	target := ref
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		target = ref[i+1:]
	} else if !looksLikeBareDigest(ref) {
		return "", errors.New("a digest-pinned reference is required (name@sha256:…)")
	}

	d := digest.Digest(target)
	if err := digestutil.ValidateSHA256(d); err != nil {
		return "", err
	}
	return d, nil
}

// looksLikeBareDigest reports whether s reads as "<sha-N algorithm>:<encoded>"
// rather than a named reference, so "sha256:…" is treated as a bare digest while
// a ref like "sha-registry.com/app:v1" still gets the digest-pinned-required
// error.
func looksLikeBareDigest(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 3 || !strings.HasPrefix(s, "sha") {
		return false
	}
	for _, r := range s[3:i] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hostPlatform() ocispec.Platform {
	return ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}
