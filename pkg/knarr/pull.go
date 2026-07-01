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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/materialize"
	"github.com/foundriesio/knarr/internal/ocilayout"
)

const registryPullUnsupported = "registry pull is not yet supported; use --from-dir"

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
	if cfg.FromDir == "" {
		return nil, errors.New(registryPullUnsupported)
	}

	root, err := parseDigestPinnedRef(ref)
	if err != nil {
		return nil, err
	}

	src, err := ocilayout.Open(cfg.FromDir)
	if err != nil {
		return nil, err
	}

	built, err := materialize.Materialize(ctx, e.obj, e.img, src, root, hostPlatform())
	if err != nil {
		return nil, err
	}
	if err := e.refs.Put(ref, built.RootDesc, time.Now(), []string{ocilayout.PlatformString(&built.Platform)}); err != nil {
		return nil, err
	}

	return &Image{
		Ref:    ref,
		Digest: root.String(),
		Size:   built.Descriptor.Size,
	}, nil
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
