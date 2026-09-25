// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ociremote fetches OCI image blobs from a registry and pushes sealed
// manifests.
package ociremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/ociseal"
)

// Options controls what Seal targets and which registry writes it performs.
type Options struct {
	// Platform selects which child of an image index to seal, as
	// "os/arch[/variant]". Required when the reference names an index and
	// rejected when it names an image manifest.
	Platform string
	Tag      string
	DryRun   bool
	// FromDir reads the image from a local OCI image layout directory
	// instead of the registry. The reference still names the repository the
	// sealed manifests are pushed to, and the registry accepts them only if
	// it already holds the blobs they reference.
	FromDir string
	// AllowNonCanonical permits sealing a non-canonical construction: its
	// deterministic fallback digest is recorded under the knarr fallback
	// annotations instead of declining. A canonical construction is sealed
	// under the standard annotations regardless of this option.
	AllowNonCanonical bool
}

// Seal computes integrity annotations for one image manifest — ref directly,
// or the Options.Platform child of the index ref names — and pushes the
// changed manifests in the same repository unless DryRun is set.
func Seal(ctx context.Context, ref string, opts Options) (ociseal.Result, error) {
	input, err := name.NewDigest(ref)
	if err != nil {
		return ociseal.Result{}, fmt.Errorf("ociremote: parse reference: %w", err)
	}
	var (
		src  ocichain.BlobSource
		root digest.Digest
	)
	if opts.FromDir != "" {
		if err := RequireSecureRegistry(input.Context()); err != nil {
			return ociseal.Result{}, err
		}
		root = digest.Digest(input.DigestStr())
		if err := digestutil.ValidateSHA256(root); err != nil {
			return ociseal.Result{}, fmt.Errorf("ociremote: %w", err)
		}
		if src, err = ocilayout.Open(opts.FromDir); err != nil {
			return ociseal.Result{}, err
		}
	} else {
		if src, root, err = Open(ctx, ref); err != nil {
			return ociseal.Result{}, err
		}
	}
	tag, err := outputTag(input.Context(), opts.Tag)
	if err != nil {
		return ociseal.Result{}, err
	}
	var want *ocispec.Platform
	if opts.Platform != "" {
		parsed, err := ggcrv1.ParsePlatform(opts.Platform)
		if err != nil {
			return ociseal.Result{}, fmt.Errorf("ociremote: parse platform: %w", err)
		}
		want = &ocispec.Platform{OS: parsed.OS, Architecture: parsed.Architecture, Variant: parsed.Variant}
	}
	result, manifests, err := ociseal.Compute(ctx, src, root, want, opts.AllowNonCanonical)
	if err != nil {
		return ociseal.Result{}, err
	}
	result.Repository = input.Context().String()
	if opts.DryRun {
		return result, nil
	}
	for _, manifest := range manifests {
		if !manifest.Changed {
			continue
		}
		if err := putManifest(ctx, input.Context().Digest(manifest.Digest.String()), manifest.Raw, manifest.MediaType); err != nil {
			return ociseal.Result{}, fmt.Errorf("ociremote: push manifest %s: %w", manifest.Digest, err)
		}
	}
	if tag != nil {
		rootManifest := manifests[len(manifests)-1]
		if err := putManifest(ctx, *tag, rootManifest.Raw, rootManifest.MediaType); err != nil {
			return ociseal.Result{}, fmt.Errorf("ociremote: push tag: %w", err)
		}
	}
	return result, nil
}

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
	return &source{repo: repo, root: root, puller: puller, transport: remote.DefaultTransport}, root, nil
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

type (
	rawManifest struct {
		data      []byte
		mediaType types.MediaType
	}

	source struct {
		repo      name.Repository
		root      digest.Digest
		puller    *remote.Puller
		transport http.RoundTripper // the base transport range requests are built on

		mu       sync.Mutex
		rangeTr  http.RoundTripper          // the authenticated transport range requests go through, built on first use
		blobURLs map[digest.Digest]*url.URL // per blob, the URL its last accepted range answer came from after a redirect
	}

	// bodySource hands an already fetched response body to OpenBlob's verifier.
	bodySource struct {
		body io.ReadCloser
	}
)

func (m rawManifest) RawManifest() ([]byte, error) {
	return m.data, nil
}

func (m rawManifest) MediaType() (types.MediaType, error) {
	return m.mediaType, nil
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

// OpenRange fetches n bytes of desc's blob starting at off with one HTTP Range
// request. A registry that ignores the Range header answers 200 with the whole
// blob; that stream is returned verified against desc, with whole=true. A 206
// answer must cover exactly the requested span of a blob of desc.Size, and a
// redirect to blob storage carries the Range header along, but not to a
// private or link-local address literal. Any other answer is an error.
//
// The URL a redirect lands on is remembered per blob for the source's
// lifetime, and later ranges of that blob are requested from it directly. An
// answer from a remembered URL that is not an acceptable 206 or 200, or a
// request that fails before any answer, forgets the URL and the range is
// re-resolved through the registry's blobs endpoint once; a failure while ctx
// is cancelled keeps the URL and returns the cancellation.
func (s *source) OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	if err := digestutil.ValidateSHA256(desc.Digest); err != nil {
		return nil, false, fmt.Errorf("ociremote: blob digest: %w", err)
	}
	if off < 0 || n < 0 || off > desc.Size || n > desc.Size-off {
		return nil, false, fmt.Errorf("ociremote: range %d+%d exceeds blob %s of size %d", off, n, desc.Digest, desc.Size)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if n == 0 {
		// A Range header cannot express an empty span; there is nothing to fetch.
		return io.NopCloser(bytes.NewReader(nil)), false, nil
	}

	tr, err := s.rangeTransport(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("ociremote: range transport for %s: %w", s.repo.Name(), err)
	}
	client := &http.Client{Transport: tr, CheckRedirect: checkRedirect}

	s.mu.Lock()
	cached := s.blobURLs[desc.Digest]
	s.mu.Unlock()
	if cached != nil {
		cachedURL := cached.String()
		resp, err := fetchRange(ctx, client, cachedURL, desc, off, n)
		if err == nil {
			var (
				rc    io.ReadCloser
				whole bool
			)
			rc, whole, err = s.openAnswer(ctx, resp, cachedURL, desc, off, n)
			if err == nil {
				return rc, whole, nil
			}
		}
		// A cancellation is nothing against the remembered URL, so it is kept
		// and the cancellation itself is returned. Any other failure forgets
		// the URL and the range is asked of the registry below; the error
		// states which answer, or which transport failure, it was.
		if cerr := ctx.Err(); cerr != nil {
			return nil, false, cerr
		}
		s.forgetBlobURL(desc.Digest, cached)
		slog.Debug("ociremote: remembered blob URL did not serve the range, asking the registry again", "digest", desc.Digest, "host", cached.Host, "err", err)
	}

	u := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", s.repo.Scheme(), s.repo.RegistryStr(), s.repo.RepositoryStr(), desc.Digest)
	resp, err := fetchRange(ctx, client, u, desc, off, n)
	if err != nil {
		return nil, false, err
	}
	return s.openAnswer(ctx, resp, u, desc, off, n)
}

// openAnswer turns the answer to a range request sent to u into the caller's
// reader: the exact span of a 206 that covers it, or the verified whole blob
// of a 200. Either is accepted, and when the answer came from another URL
// than u, after a redirect, that URL is remembered for desc's blob. Any other
// answer is an error and its body is closed.
func (s *source) openAnswer(ctx context.Context, resp *http.Response, u string, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	var (
		rc    io.ReadCloser
		whole bool
	)
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if err := checkPartialContent(resp, off, n, desc.Size); err != nil {
			return nil, false, closeBody(resp.Body, fmt.Errorf("ociremote: range of %s: %w", desc.Digest, err))
		}
		rc = &exactReader{body: resp.Body, remaining: n}
	case http.StatusOK:
		verified, err := ocichain.OpenBlob(ctx, bodySource{body: resp.Body}, desc)
		if err != nil {
			return nil, false, closeBody(resp.Body, err)
		}
		rc, whole = verified, true
	default:
		err := transport.CheckError(resp, http.StatusPartialContent, http.StatusOK)
		return nil, false, closeBody(resp.Body, notFoundOr(fmt.Errorf("ociremote: fetch range of %s: %w", desc.Digest, err), err, desc.Digest))
	}
	if final := resp.Request.URL; final.String() != u {
		s.mu.Lock()
		if s.blobURLs == nil {
			s.blobURLs = make(map[digest.Digest]*url.URL)
		}
		s.blobURLs[desc.Digest] = final
		s.mu.Unlock()
	}
	return rc, whole, nil
}

// forgetBlobURL removes failed from the remembered URLs, so the next range of
// dgst's blob is resolved through the registry again. A different URL, which
// another range remembered in the meantime, is left in place.
func (s *source) forgetBlobURL(dgst digest.Digest, failed *url.URL) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobURLs[dgst] == failed {
		delete(s.blobURLs, dgst)
	}
}

// DescribeMissingRoot renders an actionable error for a pinned root digest the
// registry does not serve.
func (s *source) DescribeMissingRoot(root digest.Digest, _ error) error {
	return fmt.Errorf("ociremote: root %s is not present in registry %s; verify the pinned digest and that the image is pushed and pullable", root, s.repo.Name())
}

// rangeTransport returns the authenticated transport range requests go through,
// building it on first use: the constructor pings the registry and performs the
// token exchange, so it runs once per source rather than once per range. A
// failed build is not remembered; the next call tries again.
func (s *source) rangeTransport(ctx context.Context) (http.RoundTripper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rangeTr != nil {
		return s.rangeTr, nil
	}
	auth, err := authn.Resolve(ctx, authn.DefaultKeychain, s.repo)
	if err != nil {
		return nil, err
	}
	tr, err := transport.NewWithContext(ctx, s.repo.Registry, auth, s.transport, []string{s.repo.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	s.rangeTr = transport.NewRetry(tr, transport.WithRetryStatusCodes(retryStatusCodes...))
	return s.rangeTr, nil
}

// fetchRange sends one Range request for off+n of desc's blob to u and returns
// the answer, whatever its status. A failure before any answer is wrapped with
// its URL's query redacted.
func fetchRange(ctx context.Context, client *http.Client, u string, desc ocispec.Descriptor, off, n int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("ociremote: range request for %s: %w", desc.Digest, err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ociremote: fetch range of %s: %w", desc.Digest, redactURL(err))
	}
	return resp, nil
}

// retryStatusCodes are the answers a range request is retried on: the
// registry is momentarily unavailable or rate-limiting, and the request has
// no body to replay.
var retryStatusCodes = []int{
	http.StatusRequestTimeout,
	http.StatusTooManyRequests,
	http.StatusInternalServerError,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

// checkRedirect refuses a redirect from one host to a private, loopback or
// link-local IP address: a registry must not be able to send a blob request to
// a service inside the device's own network. Same-host redirects and host
// names are allowed, as is the client's default hop limit.
//
// It also drops the Referer the client sets on a redirected request from the
// URL the request came from: that URL may be a blob storage URL whose query
// carries signed credentials, and no request sent from here needs the header.
func checkRedirect(req *http.Request, via []*http.Request) error {
	req.Header.Del("Referer")
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	dest := req.URL.Hostname()
	if dest == via[0].URL.Hostname() {
		return nil
	}
	addr, ok := parseAddr(dest)
	if !ok {
		return nil
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("redirect to %s refused", dest)
	}
	return nil
}

// parseAddr parses a host as the IP address the dialer would connect to: a
// zone suffix is dropped, an IPv4-mapped IPv6 address is the IPv4 address it
// maps, and the legacy IPv4 spellings the resolver still accepts (fewer than
// four parts, hex or octal parts, e.g. 127.1 or 0x7f000001) are expanded.
func parseAddr(host string) (netip.Addr, bool) {
	host, _, _ = strings.Cut(host, "%")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap(), true
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	var b [4]byte
	for i, part := range parts[:len(parts)-1] {
		v, err := strconv.ParseUint(part, 0, 8)
		if err != nil {
			return netip.Addr{}, false
		}
		b[i] = byte(v)
	}
	// The last part fills the remaining bytes: 127.1 is 127.0.0.1.
	last, err := strconv.ParseUint(parts[len(parts)-1], 0, 8*(5-len(parts)))
	if err != nil {
		return netip.Addr{}, false
	}
	for i := len(parts) - 1; i < 4; i++ {
		b[i] = byte(last >> (8 * (3 - i)) & 0xff)
	}
	return netip.AddrFrom4(b), true
}

// redactURL strips the query of the URL a request error carries, which after
// a redirect to blob storage holds the storage's signed credentials. A
// Location the client could not parse is quoted whole in the error it
// returns, so that message is replaced.
func redactURL(err error) error {
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return err
	}
	inner := uerr.Err
	if strings.Contains(inner.Error(), "Location header") {
		inner = errors.New("failed to parse Location header (redacted)")
	}
	u, perr := url.Parse(uerr.URL)
	if perr != nil {
		return &url.Error{Op: uerr.Op, URL: "REDACTED", Err: inner}
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.User = nil
	return &url.Error{Op: uerr.Op, URL: u.String(), Err: inner}
}

func (b bodySource) Open(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return b.body, nil
}

// checkPartialContent verifies that a 206 answer covers exactly the n bytes
// requested at off of a blob of the given size. The body's length is checked
// when the answer states it; a chunked answer states none, and exactReader
// checks that the bytes arrive.
func checkPartialContent(resp *http.Response, off, n, size int64) error {
	contentRange := resp.Header.Get("Content-Range")
	first, last, total, err := parseContentRange(contentRange)
	if err != nil {
		return fmt.Errorf("malformed Content-Range %q: %w", contentRange, err)
	}
	if first != off || last != off+n-1 || total != size {
		return fmt.Errorf("Content-Range %q does not match the requested bytes %d-%d/%d", contentRange, off, off+n-1, size)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != n {
		return fmt.Errorf("Content-Length %d does not match the requested %d bytes", resp.ContentLength, n)
	}
	return nil
}

// parseContentRange parses "bytes first-last/total". The unit is compared
// without regard to case and the numbers are unsigned decimals; anything else
// in the value is an error.
func parseContentRange(value string) (first, last, total int64, err error) {
	unit, rest, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, errors.New("not a byte range")
	}
	span, totalText, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, errors.New("no total length")
	}
	firstText, lastText, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, errors.New("no span")
	}
	for _, field := range []struct {
		text string
		into *int64
	}{{firstText, &first}, {lastText, &last}, {totalText, &total}} {
		v, err := strconv.ParseUint(field.text, 10, 63)
		if err != nil {
			return 0, 0, 0, err
		}
		*field.into = int64(v)
	}
	return first, last, total, nil
}

// exactReader reads the requested bytes of a 206 body and fails, instead of
// returning io.EOF, when the body ends before them. Bytes past them are not
// read: the caller authenticates what it reads.
type exactReader struct {
	body      io.ReadCloser
	remaining int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.body.Read(p)
	e.remaining -= int64(n)
	if errors.Is(err, io.EOF) && e.remaining > 0 {
		return n, fmt.Errorf("ociremote: range body %d bytes short: %w", e.remaining, io.ErrUnexpectedEOF)
	}
	return n, err
}

func (e *exactReader) Close() error {
	return e.body.Close()
}

// closeBody closes a response body that is not handed to the caller and joins
// a close failure onto err.
func closeBody(body io.Closer, err error) error {
	if closeErr := body.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("ociremote: close response body: %w", closeErr))
	}
	return err
}

func putManifest(ctx context.Context, ref name.Reference, raw []byte, mediaType string) error {
	return remote.Put(ref, rawManifest{data: raw, mediaType: types.MediaType(mediaType)},
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
}

func outputTag(repo name.Repository, value string) (*name.Tag, error) {
	if value == "" {
		return nil, nil
	}
	if strings.ContainsAny(value, "/@") {
		return nil, errors.New("ociremote: --tag must be a tag name in the input repository")
	}
	tag, err := name.NewTag(repo.Name() + ":" + value)
	if err != nil {
		return nil, fmt.Errorf("ociremote: parse --tag: %w", err)
	}
	return &tag, nil
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
