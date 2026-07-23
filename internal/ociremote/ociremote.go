// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ociremote fetches OCI image blobs from a registry.
package ociremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/ocichain"
)

// Open builds a registry-backed BlobSource for ref and returns its root
// digest. The reference must name a repository and either pin a sha256 digest
// (name@sha256:...) or carry a tag (name:tag, defaulting to :latest); a bare
// digest is rejected because a registry fetch needs a repository. A pinned
// digest is the trust anchor and costs no network I/O here: the root and its
// blobs are fetched lazily when the returned source is opened. A tag is a
// mutable alias, so Open resolves it to the manifest digest the registry
// currently serves with one round-trip; that resolution trusts the registry,
// while everything fetched afterwards is verified against the resolved digest.
// The registry must use HTTPS unless it is a loopback address (localhost or a
// loopback IP), where plain HTTP is allowed: for a pinned digest content
// integrity comes from the digest, not the channel, and a loopback registry
// is standard tooling.
func Open(ctx context.Context, ref string) (ocichain.BlobSource, digest.Digest, error) {
	if _, err := digest.Parse(ref); err == nil {
		return nil, "", fmt.Errorf("ociremote: reference %q is a bare digest; a registry fetch needs a repository (name@sha256:...)", ref)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, "", fmt.Errorf("ociremote: parse reference %q: %w", ref, err)
	}
	repo := parsed.Context()
	if err := RequireSecureRegistry(repo); err != nil {
		return nil, "", err
	}
	puller, err := remote.NewPuller(remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, "", fmt.Errorf("ociremote: init puller: %w", err)
	}
	var root digest.Digest
	switch r := parsed.(type) {
	case name.Digest:
		root = digest.Digest(r.DigestStr())
	case name.Tag:
		desc, err := puller.Head(ctx, r)
		if err != nil {
			return nil, "", fmt.Errorf("ociremote: resolve tag %s: %w", r.Name(), err)
		}
		root = digest.Digest(desc.Digest.String())
	default:
		return nil, "", fmt.Errorf("ociremote: reference %q is neither digest-pinned nor tagged", ref)
	}
	if err := digestutil.ValidateSHA256(root); err != nil {
		return nil, "", fmt.Errorf("ociremote: %w", err)
	}
	return &source{repo: repo, root: root, puller: puller}, root, nil
}

type source struct {
	repo   name.Repository
	root   digest.Digest
	puller *remote.Puller
}

// Open fetches desc's raw bytes from the registry. A manifest or index
// descriptor is fetched from the manifests endpoint; every other blob (config,
// layers) is fetched from the blobs endpoint. A descriptor with no media type is
// routed to the manifests endpoint because ocichain.Resolve only supplies an
// empty media type for a manifest. A manifest response is buffered in memory by
// the puller, which caps it at go-containerregistry's 100 MiB manifest limit;
// any tighter bound a caller applies to the returned reader takes effect only
// after that buffering.
func (s *source) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if err := digestutil.ValidateSHA256(desc.Digest); err != nil {
		return nil, fmt.Errorf("ociremote: blob digest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ref := s.repo.Digest(desc.Digest.String())
	if desc.Digest == s.root || desc.MediaType == "" || isManifestOrIndex(desc.MediaType) {
		d, err := s.puller.Get(ctx, ref)
		if err != nil {
			return nil, notFoundOr(fmt.Errorf("ociremote: fetch manifest %s: %w", desc.Digest, err), err, desc.Digest)
		}
		return io.NopCloser(bytes.NewReader(d.Manifest)), nil
	}

	layer, err := s.puller.Layer(ctx, ref)
	if err != nil {
		return nil, notFoundOr(fmt.Errorf("ociremote: fetch blob %s: %w", desc.Digest, err), err, desc.Digest)
	}
	rc, err := layer.Compressed()
	if err != nil {
		return nil, notFoundOr(fmt.Errorf("ociremote: read blob %s: %w", desc.Digest, err), err, desc.Digest)
	}
	return rc, nil
}

// DescribeMissingRoot renders an actionable error for a pinned root digest the
// registry does not serve.
func (s *source) DescribeMissingRoot(root digest.Digest, _ error) error {
	return fmt.Errorf("ociremote: root %s is not present in registry %s; verify the pinned digest and that the image is pushed and pullable", root, s.repo.Name())
}

// RequireSecureRegistry rejects a repository whose registry would be contacted
// over plain HTTP, unless it is a loopback address (localhost or a loopback
// IP): content integrity comes from a pinned digest, not the channel, and a
// loopback registry is standard tooling. Every path that talks to a registry
// must apply this policy, whether it fetches or only pushes.
func RequireSecureRegistry(repo name.Repository) error {
	if repo.Scheme() != "https" && !isLoopback(repo.RegistryStr()) {
		return fmt.Errorf("ociremote: registry %s does not use HTTPS", repo.RegistryStr())
	}
	return nil
}

// isLoopback reports whether registry (a host or host:port) is localhost or a
// loopback IP, the one case where a plain-HTTP registry is accepted.
func isLoopback(registry string) bool {
	host := registry
	if h, _, err := net.SplitHostPort(registry); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isManifestOrIndex reports whether mediaType is an OCI or Docker manifest or
// index type, which the registry serves from its manifests endpoint.
func isManifestOrIndex(mediaType string) bool {
	switch mediaType {
	case ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	default:
		return false
	}
}

// notFoundOr tags a registry 404 with os.ErrNotExist only when the failed
// request path contains dgst, so setup failures such as the /v2/ ping are not
// confused with a missing blob. This gate can miss redirected blob 404s whose
// final URL does not contain the digest, but it is only load-bearing for the
// root, which is fetched from /v2/<repo>/manifests/<digest> and never
// redirected.
func notFoundOr(wrapped, raw error, dgst digest.Digest) error {
	var terr *transport.Error
	if errors.As(raw, &terr) && terr.StatusCode == http.StatusNotFound &&
		terr.Request != nil && strings.Contains(terr.Request.URL.Path, dgst.String()) {
		return errors.Join(wrapped, os.ErrNotExist)
	}
	return wrapped
}
