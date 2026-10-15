// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ociremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/octest"
)

func TestOpenRejectsBareDigestOrPlainHTTP(t *testing.T) {
	for _, ref := range []string{
		"sha256:" + strings.Repeat("a", 64),
		"192.168.1.10:5000/team/app@sha256:" + strings.Repeat("e", 64), // plain HTTP but not loopback
		"192.168.1.10:5000/team/app:latest",                            // ditto, rejected before tag resolution
		"not a ref",
	} {
		if _, _, err := Open(context.Background(), ref); err == nil {
			t.Errorf("Open(%q) = nil error, want error", ref)
		}
	}
}

func TestOpenAllowsPlainHTTPLoopback(t *testing.T) {
	dgst := "sha256:" + strings.Repeat("e", 64)
	for _, ref := range []string{
		"localhost:5000/team/app@" + dgst,
		"127.0.0.1:5000/team/app@" + dgst,
	} {
		if _, _, err := Open(context.Background(), ref); err != nil {
			t.Errorf("Open(%q) = %v, want nil", ref, err)
		}
	}
}

func TestOpenReturnsPinnedRoot(t *testing.T) {
	dgst := "sha256:" + strings.Repeat("b", 64)
	_, root, err := Open(context.Background(), "registry.example.com/team/app@"+dgst)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if root.String() != dgst {
		t.Errorf("root = %s, want %s", root, dgst)
	}
}

func TestOpenResolvesTag(t *testing.T) {
	host, tr := startRegistry(t)
	img := hostImage(t, 1)
	want := writeImage(t, host, tr, "team/app", img)
	useRemoteDefaultTransport(t, tr)

	_, root, err := Open(context.Background(), host+"/team/app:latest")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if root != want {
		t.Errorf("resolved root = %s, want %s", root, want)
	}

	if _, _, err := Open(context.Background(), host+"/team/app:absent"); err == nil {
		t.Error("Open(absent tag) = nil error, want error")
	} else if !strings.Contains(err.Error(), "resolve tag") {
		t.Errorf("Open(absent tag) error = %v, want resolve-tag message", err)
	}
}

func TestSourceOpenRoutesAndFetches(t *testing.T) {
	host, tr := startRegistry(t)
	img := hostImage(t, 2)
	rootDigest := writeImage(t, host, tr, "team/app", img)
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}

	src := testSource(t, host, "team/app", rootDigest, tr)
	ctx := context.Background()

	rawManifest, err := img.RawManifest()
	if err != nil {
		t.Fatalf("raw manifest: %v", err)
	}
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		t.Fatalf("raw config: %v", err)
	}

	assertOpenBytes(t, src, ctx, ocispec.Descriptor{Digest: rootDigest}, rawManifest)
	assertOpenBytes(t, src, ctx, descOf(manifest.Config), rawConfig)

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	for i, layer := range layers {
		rawLayer := readCompressedLayer(t, layer)
		assertOpenBytes(t, src, ctx, descOf(manifest.Layers[i]), rawLayer)
	}

	absent := ocispec.Descriptor{
		Digest:    digest.Digest("sha256:" + strings.Repeat("c", 64)),
		MediaType: ocispec.MediaTypeImageConfig,
	}
	if _, err := src.Open(ctx, absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open(absent blob) error = %v, want os.ErrNotExist", err)
	}
	absent.MediaType = ocispec.MediaTypeImageManifest
	if _, err := src.Open(ctx, absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open(absent manifest) error = %v, want os.ErrNotExist", err)
	}
}

func TestResolveSingleManifest(t *testing.T) {
	host, tr := startRegistry(t)
	img := hostImage(t, 2)
	root := writeImage(t, host, tr, "team/single", img)

	src := testSource(t, host, "team/single", root, tr)
	res, err := ocichain.Resolve(context.Background(), src, root, hostPlatform())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.ManifestDesc.Digest != root {
		t.Errorf("manifest desc = %s, want root %s", res.ManifestDesc.Digest, root)
	}
	if len(res.DiffIDs) != 2 {
		t.Errorf("DiffIDs = %d, want 2", len(res.DiffIDs))
	}
}

func TestResolveIndexSelectsHostChild(t *testing.T) {
	host, tr := startRegistry(t)
	img := hostImage(t, 2)
	idx := hostIndex(img)
	id, err := idx.Digest()
	if err != nil {
		t.Fatalf("index digest: %v", err)
	}
	tag := testTag(t, host, "team/multi")
	if err := remote.WriteIndex(tag, idx, remote.WithTransport(tr)); err != nil {
		t.Fatalf("push index: %v", err)
	}

	root := digest.Digest(id.String())
	src := testSource(t, host, "team/multi", root, tr)
	res, err := ocichain.Resolve(context.Background(), src, root, hostPlatform())
	if err != nil {
		t.Fatalf("Resolve index: %v", err)
	}
	child, err := img.Digest()
	if err != nil {
		t.Fatalf("child digest: %v", err)
	}
	if res.ManifestDesc.Digest.String() != child.String() {
		t.Errorf("selected manifest = %s, want child %s", res.ManifestDesc.Digest, child)
	}
}

func TestResolveMissingRootDescribed(t *testing.T) {
	host, tr := startRegistry(t)
	absent := digest.Digest("sha256:" + strings.Repeat("d", 64))
	src := testSource(t, host, "team/empty", absent, tr)
	_, err := ocichain.Resolve(context.Background(), src, absent, hostPlatform())
	if err == nil || !strings.Contains(err.Error(), "not present in registry") {
		t.Errorf("Resolve(absent root) = %v, want DescribeMissingRoot message", err)
	}
}

func startRegistry(t *testing.T) (host string, tr http.RoundTripper) {
	t.Helper()
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	return "registry.example.com", reg.Transport()
}

// useRemoteDefaultTransport routes Open's own puller, which tests cannot
// inject a transport into, through the in-memory registry.
func useRemoteDefaultTransport(t *testing.T, tr http.RoundTripper) {
	t.Helper()
	old := remote.DefaultTransport
	remote.DefaultTransport = tr
	t.Cleanup(func() {
		remote.DefaultTransport = old
	})
}

func hostImage(t *testing.T, layers int64) v1.Image {
	t.Helper()
	img, err := random.Image(1024, layers)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	cfg.OS = runtime.GOOS
	cfg.Architecture = runtime.GOARCH
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("set platform: %v", err)
	}
	return img
}

func hostIndex(img v1.Image) v1.ImageIndex {
	return mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add:        img,
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}},
	})
}

func writeImage(t *testing.T, host string, tr http.RoundTripper, repo string, img v1.Image) digest.Digest {
	t.Helper()
	if err := remote.Write(testTag(t, host, repo), img, remote.WithTransport(tr)); err != nil {
		t.Fatalf("push image: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}
	return digest.Digest(d.String())
}

func testTag(t *testing.T, host, repo string) name.Tag {
	t.Helper()
	tag, err := name.NewTag(host + "/" + repo + ":latest")
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	return tag
}

func testSource(t *testing.T, host, repo string, root digest.Digest, tr http.RoundTripper) *source {
	t.Helper()
	r, err := name.NewRepository(host + "/" + repo)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	puller, err := remote.NewPuller(remote.WithTransport(tr))
	if err != nil {
		t.Fatalf("puller: %v", err)
	}
	return &source{repo: r, root: root, puller: puller, transport: tr}
}

func hostPlatform() ocispec.Platform {
	return ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}

func descOf(d v1.Descriptor) ocispec.Descriptor {
	return ocispec.Descriptor{Digest: digest.Digest(d.Digest.String()), MediaType: string(d.MediaType), Size: d.Size}
}

func readCompressedLayer(t *testing.T, layer v1.Layer) []byte {
	t.Helper()
	rc, err := layer.Compressed()
	if err != nil {
		t.Fatalf("layer compressed: %v", err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("close layer: %v", err)
		}
	}()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read layer: %v", err)
	}
	return data
}

func assertOpenBytes(t *testing.T, src *source, ctx context.Context, desc ocispec.Descriptor, want []byte) {
	t.Helper()
	rc, err := src.Open(ctx, desc)
	if err != nil {
		t.Fatalf("Open(%s): %v", desc.Digest, err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("close %s: %v", desc.Digest, err)
		}
	}()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", desc.Digest, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Open(%s) returned %d bytes, want %d", desc.Digest, len(got), len(want))
	}
}
