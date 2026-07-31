// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"bytes"
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
	"github.com/foundriesio/knarr/internal/ociseal"
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

	// The store lock is held shared across the whole pull: objects are written
	// before the ref record that makes them wanted, and a concurrent sweep in
	// that window would eat the staged image. The wait is not interruptible;
	// the ctx gate right after is the single pre-mutation cancellation check.
	unlock, err := e.lockStoreShared()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	src, root, err := pullSource(ctx, ref, cfg)
	if err != nil {
		return nil, err
	}

	platform := hostPlatform()
	// Descend the pinned root to the image identity before anything is looked up
	// or recorded, so an index-rooted and a manifest-rooted pull of the same
	// image converge on one key.
	desc, fetched, err := e.pullDescent(ctx, src, root, platform)
	if err != nil {
		return nil, err
	}

	if cached, ok, err := verifiedPulledImage(ctx, e.obj, e.img, desc.Identity, platform); err != nil {
		return nil, err
	} else if ok {
		normalized, err := normalizeRef(ref)
		if err != nil {
			return nil, err
		}
		// Store the root the descent had to fetch: the identity is already local
		// on this path, but the document the reference resolved to is not, and
		// the record about to be written names it. Sync before the record so no
		// ref points at a blob a crash could lose.
		if fetched != nil {
			if err := e.obj.WriteBlob(ctx, root, bytes.NewReader(fetched)); err != nil {
				return nil, err
			}
			if err := e.obj.Sync(); err != nil {
				return nil, err
			}
		}
		if err := e.refs.Put(normalized, desc.Root, desc.Identity, time.Now(), []string{ocilayout.PlatformString(&cached.platform)}); err != nil {
			return nil, err
		}
		return &Image{
			Ref:    ref,
			Digest: desc.Identity.Digest.String(),
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
	if err := e.refs.Put(normalized, built.RootDesc, built.Identity, time.Now(), []string{ocilayout.PlatformString(&built.Platform)}); err != nil {
		return nil, err
	}

	return &Image{
		Ref:    ref,
		Digest: built.Identity.Digest.String(),
		Size:   built.Descriptor.Size,
	}, nil
}

// pullSource picks where a pull reads its blobs from and the root digest it
// anchors on: a registry, which resolves a tag to the digest it currently
// serves; a local OCI image layout directory; or a source the caller supplied.
// The latter two fetch nothing and so cannot resolve a tag, which is why the
// reference must pin the root digest — unless the caller resolved it and passed
// it in, in which case that digest is the anchor and the reference is left to
// the ref store as an alias for it.
func pullSource(ctx context.Context, ref string, cfg PullOptions) (ocichain.BlobSource, digest.Digest, error) {
	switch {
	case cfg.BlobSource != nil && cfg.FromDir != "":
		return nil, "", errors.New("a blob source and a layout directory are mutually exclusive")
	case cfg.BlobSource == nil && cfg.ResolvedRoot != "":
		return nil, "", errors.New("a resolved root digest requires a blob source")

	case cfg.BlobSource != nil:
		if cfg.ResolvedRoot == "" {
			root, err := parseDigestPinnedRef(ref)
			return cfg.BlobSource, root, err
		}
		if err := digestutil.ValidateSHA256(cfg.ResolvedRoot); err != nil {
			return nil, "", err
		}
		return cfg.BlobSource, cfg.ResolvedRoot, nil

	case cfg.FromDir != "":
		root, err := parseDigestPinnedRef(ref)
		if err != nil {
			return nil, "", err
		}
		src, err := ocilayout.Open(cfg.FromDir)
		if err != nil {
			return nil, "", err
		}
		return src, root, nil

	default:
		return ociremote.Open(ctx, ref)
	}
}

// descended is a pinned root resolved one level down to the image it names.
type descended struct {
	// Root is the descriptor of the root blob as it was read: the pinned digest
	// and the length its bytes actually have.
	Root ocispec.Descriptor
	// Identity is the descriptor of the platform image manifest the image is
	// keyed by -- the selected index entry, carrying the size that index
	// declares for it, or Root itself when the root is already a manifest.
	Identity ocispec.Descriptor
}

// pullDescent descends root for a pull, reading the root blob from the object
// store when it is already stored and from the pull source otherwise. The
// returned bytes are non-nil exactly when they came from the source, so the
// caller can store what the local store is missing. A local blob that fails to
// read or verify is not fatal here: the source is asked for the same digest,
// which heals a damaged or truncated local copy.
func (e *engine) pullDescent(ctx context.Context, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform) (descended, []byte, error) {
	desc, _, err := descendRoot(ctx, casBlobSource{store: e.obj}, root, platform)
	if err == nil {
		return desc, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return descended{}, nil, err
	}
	desc, rootBytes, err := descendRoot(ctx, src, root, platform)
	if err != nil {
		return descended{}, nil, err
	}
	return desc, rootBytes, nil
}

// descendRoot reads and digest-verifies root's blob through src and resolves it
// to the image it names for platform: the manifest the index selects for that
// platform, or root itself when root is a manifest. Exactly one level is
// descended -- a nested index entry is returned as it stands -- and no manifest
// is read, so the descriptor is taken from bytes that hash to the digest the
// caller pinned. The verified root bytes are returned alongside.
func descendRoot(ctx context.Context, src ocichain.BlobSource, root digest.Digest, platform ocispec.Platform) (descended, []byte, error) {
	rootBytes, err := ocichain.ReadRootBlob(ctx, src, root)
	if err != nil {
		return descended{}, nil, err
	}
	rootDesc := ocispec.Descriptor{Digest: root, Size: int64(len(rootBytes))}

	// Gate on the structure before selecting: a bare manifest unmarshals into an
	// index with no entries, which would read as a platform that is not offered.
	isIndex, err := ocichain.IsIndex(rootBytes)
	if err != nil {
		return descended{}, nil, err
	}
	if !isIndex {
		return descended{Root: rootDesc, Identity: rootDesc}, rootBytes, nil
	}
	identity, err := ocichain.SelectManifestDesc(rootBytes, platform)
	if err != nil {
		return descended{}, nil, err
	}
	return descended{Root: rootDesc, Identity: identity}, rootBytes, nil
}

type cachedImage struct {
	platform   ocispec.Platform
	descriptor ocispec.Descriptor
}

// verifiedPulledImage reports whether (identity, platform) is already
// materialized and trustworthy: a binding exists, the bound composefs object
// measures to the recorded fs-verity digest, the identity manifest is stored at
// the length its descriptor declares, and the metadata chain resolves from the
// local store. Both the binding lookup and the local resolution key on the
// identity digest, so a store that holds the image under it is a hit however
// the reference that led here was written. Every failure reports "not cached"
// rather than an error, so the caller falls through to a full pull, which
// rewrites the same content idempotently and heals whatever was stale or
// missing — except a canceled context or a sealing-verification failure, both
// of which propagate as errors; a sealing mismatch signals that the recorded
// content and its claimed integrity annotation disagree rather than merely
// being stale.
func verifiedPulledImage(ctx context.Context, obj *objectstore.Store, imgStore *imagestore.Store, identity ocispec.Descriptor, platform ocispec.Platform) (cachedImage, bool, error) {
	if err := ctx.Err(); err != nil {
		return cachedImage{}, false, err
	}
	b, err := imgStore.Get(identity.Digest, platform)
	if err != nil {
		return cachedImage{}, false, nil
	}
	measured, err := obj.VerityDigest(b.Descriptor.Digest)
	if err != nil || measured != b.FsverityDigest {
		return cachedImage{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return cachedImage{}, false, err
	}
	src := casBlobSource{store: obj}
	// The identity descriptor carries the size the document that named it
	// declared, so read the manifest under that bound first: a fresh pull reads
	// it the same way and would reject a disagreeing length, and a cache hit
	// must not be the laxer of the two.
	if _, err := ocichain.ReadMetaBlob(ctx, src, identity); err != nil {
		if err := ctx.Err(); err != nil {
			return cachedImage{}, false, err
		}
		return cachedImage{}, false, nil
	}
	// Resolve reads that manifest again as a pinned root, bounded by the metadata
	// cap rather than by the declared size, so an image whose manifest is larger
	// than the cap — accepted by a fresh pull, which reads it under its declared
	// size — misses here and is rebuilt on every repeat pull. The miss is the
	// safe direction: a cache hit is never the laxer read of the two.
	res, err := ocichain.Resolve(ctx, src, identity.Digest, platform)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return cachedImage{}, false, err
		}
		return cachedImage{}, false, nil
	}
	if err := ociseal.VerifyAnnotations(ctx, obj, res, b.FsverityDigest, b.Canonical); err != nil {
		return cachedImage{}, false, fmt.Errorf("verify cached sealing annotations: %w", err)
	}
	return cachedImage{platform: res.Platform, descriptor: b.Descriptor}, true, nil
}

// localAnchor classifies ref and returns the digest it anchors on locally,
// reading no blob: the pinned digest as typed (pinned true), or, for any other
// ref string, the identity its most recent pull recorded under the normalized
// form, so a pulled tag works with the verbs that read only local state
// regardless of which equivalent alias named it. Resolution never touches the
// network. The digest is empty only when the reference itself cannot be
// resolved: a malformed digest pin — a ref that reads as a digest-pin attempt
// (it contains "@" or is a bare digest) reports its parse error instead of
// falling through, because pull can never have recorded such a string — or a
// name no pull recorded.
func (e *engine) localAnchor(ref string) (root digest.Digest, pinned bool, err error) {
	root, perr := parseDigestPinnedRef(ref)
	if perr == nil {
		return root, true, nil
	}
	if strings.ContainsRune(ref, '@') || looksLikeBareDigest(ref) {
		return "", false, perr
	}
	normalized, err := normalizeRef(ref)
	if err != nil {
		return "", false, err
	}
	rec, err := e.refs.Get(normalized)
	if errors.Is(err, refstore.ErrNotFound) {
		return "", false, fmt.Errorf("not digest-pinned and not recorded by a pull; run `knarr pull` first or pin a digest (name@sha256:…): %w", errors.Join(ErrNotFound, err))
	}
	if err != nil {
		return "", false, err
	}
	return rec.Identity.Digest, false, nil
}

// resolveLocalRef returns the identity digest ref names — the platform image
// manifest every local lookup and verification keys on. A pinned digest is
// descended through the object store: a pinned index yields the manifest it
// offers for the host platform, and a pinned manifest yields itself; any
// other ref resolves through the reference record with no blob read. A
// reference that does name a digest always yields one, and a non-nil error
// alongside it reports why the descent stopped there: the root blob is
// absent, unreadable, or does not hash to its digest. Each verb decides which
// of those reasons matters to it.
func (e *engine) resolveLocalRef(ctx context.Context, ref string) (digest.Digest, error) {
	root, pinned, err := e.localAnchor(ref)
	if root == "" || !pinned {
		return root, err
	}
	desc, _, err := descendRoot(ctx, casBlobSource{store: e.obj}, root, hostPlatform())
	if err != nil {
		return root, err
	}
	return desc.Identity.Digest, nil
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
