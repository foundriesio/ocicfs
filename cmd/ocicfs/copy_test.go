// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/octest"
)

var (
	copyAMD64 = ocispec.Platform{OS: "linux", Architecture: "amd64"}
	copyARM64 = ocispec.Platform{OS: "linux", Architecture: "arm64"}
)

// copyIndex pushes a two-platform image under repo:tag to the in-memory
// registry and returns its layout; the layout's Manifest, Config and Layers
// describe the amd64 image. The arm64 manifest's digest is returned too.
func copyIndex(t *testing.T, reg *octest.Registry, repo, tag string) (octest.Layout, digest.Digest) {
	t.Helper()
	spec := func(p ocispec.Platform, body string) octest.ImageSpec {
		return octest.ImageSpec{Platform: p, Layers: []octest.LayerSpec{
			{Files: []octest.File{{Name: "etc", Dir: true}, {Name: "etc/platform", Mode: 0o644, Body: body}}},
			{Files: []octest.File{{Name: "etc/app", Mode: 0o644, Body: body + " app"}}},
		}}
	}
	lay, err := octest.BuildImage(t.TempDir(), spec(copyAMD64, "amd64"), spec(copyARM64, "arm64"))
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	path, err := ggcrlayout.FromPath(lay.Dir)
	if err != nil {
		t.Fatalf("layout.FromPath: %v", err)
	}
	idx, err := path.ImageIndex()
	if err != nil {
		t.Fatalf("layout.ImageIndex: %v", err)
	}
	if d, err := idx.Digest(); err != nil || d.String() != lay.Root.String() {
		t.Fatalf("pushed index digest = %v, %v; want the layout root %s", d, err, lay.Root)
	}
	ref, err := name.NewTag(repo + ":" + tag)
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if err := remote.WriteIndex(ref, idx, remote.WithTransport(reg.Transport())); err != nil {
		t.Fatalf("remote.WriteIndex: %v", err)
	}
	m, err := idx.IndexManifest()
	if err != nil {
		t.Fatalf("IndexManifest: %v", err)
	}
	var arm64 digest.Digest
	for _, d := range m.Manifests {
		if d.Platform != nil && d.Platform.Architecture == "arm64" {
			arm64 = digest.Digest(d.Digest.String())
		}
	}
	return lay, arm64
}

// copyRegistry starts an in-memory registry that ocicfs copy reaches through
// the default transport.
func copyRegistry(t *testing.T) *octest.Registry {
	t.Helper()
	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	old := remote.DefaultTransport
	remote.DefaultTransport = reg.Transport()
	t.Cleanup(func() { remote.DefaultTransport = old })
	return reg
}

// readArchive returns the regular files of the tar at p by name.
func readArchive(t *testing.T, p string) map[string][]byte {
	t.Helper()
	f, err := os.Open(p) // #nosec G304 -- test archive path
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = f.Close() }()
	files := make(map[string][]byte)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if _, dup := files[hdr.Name]; dup {
			t.Errorf("archive holds %s twice", hdr.Name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = b
	}
}

// archiveEntries returns the index.json entries of an archive's files,
// checking that every blob hashes to its name.
func archiveEntries(t *testing.T, files map[string][]byte) []ocispec.Descriptor {
	t.Helper()
	for name, b := range files {
		if hex, ok := strings.CutPrefix(name, "blobs/sha256/"); ok && digest.SHA256.FromBytes(b).Encoded() != hex {
			t.Errorf("blob %s does not hash to its name", name)
		}
	}
	if _, ok := files[ocispec.ImageLayoutFile]; !ok {
		t.Errorf("archive has no %s", ocispec.ImageLayoutFile)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(files[ocispec.ImageIndexFile], &idx); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	return idx.Manifests
}

func checkEntry(t *testing.T, e ocispec.Descriptor, d digest.Digest, mediaType, name string) {
	t.Helper()
	if e.Digest != d || e.MediaType != mediaType {
		t.Errorf("entry = %s %s, want %s %s", e.MediaType, e.Digest, mediaType, d)
	}
	for _, key := range []string{"io.containerd.image.name", ocispec.AnnotationRefName} {
		if got := e.Annotations[key]; got != name {
			t.Errorf("entry %s %s = %q, want %q", d, key, got, name)
		}
	}
}

func TestCopyIndexRoot(t *testing.T) {
	reg := copyRegistry(t)
	const repo = "registry.example.com/team/app"
	lay, arm64 := copyIndex(t, reg, repo, "1.0")
	out := filepath.Join(t.TempDir(), "app.tar")

	var stdout bytes.Buffer
	if err := copyArchive(context.Background(), &stdout, out, []string{repo + ":1.0"}, copyAMD64); err != nil {
		t.Fatalf("copyArchive: %v", err)
	}
	files := readArchive(t, out)
	entries := archiveEntries(t, files)
	if len(entries) != 2 {
		t.Fatalf("index.json lists %d entries, want 2: %+v", len(entries), entries)
	}
	checkEntry(t, entries[0], lay.Root, ocispec.MediaTypeImageIndex, repo+":1.0")
	checkEntry(t, entries[1], lay.Manifest, ocispec.MediaTypeImageManifest, repo+"@"+lay.Manifest.String())
	if p := entries[1].Platform; p == nil || p.Architecture != "amd64" {
		t.Errorf("platform entry platform = %+v, want linux/amd64", p)
	}
	want := map[string]bool{
		ocispec.ImageLayoutFile: true, ocispec.ImageIndexFile: true,
		blobPath(lay.Root): true, blobPath(lay.Manifest): true, blobPath(lay.Config): true,
	}
	for _, l := range lay.Layers {
		want[blobPath(l)] = true
	}
	for name := range files {
		if !want[name] {
			t.Errorf("archive holds %s, which the amd64 image does not use", name)
		}
	}
	for name := range want {
		if _, ok := files[name]; !ok {
			t.Errorf("archive lacks %s", name)
		}
	}
	if _, ok := files[blobPath(arm64)]; ok {
		t.Error("archive holds the arm64 manifest")
	}
	wantOut := repo + ":1.0\t" + lay.Root.String() + "\n" + repo + "@" + lay.Manifest.String() + "\t" + lay.Manifest.String() + "\n"
	if stdout.String() != wantOut {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantOut)
	}

	src, err := ocilayout.OpenTar(out)
	if err != nil {
		t.Fatalf("OpenTar: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	for _, pin := range []digest.Digest{lay.Root, lay.Manifest} {
		res, err := ocichain.Resolve(context.Background(), src, pin, copyAMD64)
		if err != nil {
			t.Fatalf("Resolve the archive by %s: %v", pin, err)
		}
		for _, l := range res.Manifest.Layers {
			rc, err := ocichain.OpenBlob(context.Background(), src, l)
			if err != nil {
				t.Fatalf("OpenBlob %s: %v", l.Digest, err)
			}
			if _, err := io.Copy(io.Discard, rc); err != nil {
				t.Errorf("read layer %s: %v", l.Digest, err)
			}
			_ = rc.Close()
		}
	}
	index, err := src.IndexJSON()
	if err != nil {
		t.Fatalf("IndexJSON: %v", err)
	}
	children := func(desc ocispec.Descriptor) ([]digest.Digest, error) {
		return indexChildren(context.Background(), src, desc)
	}
	got, err := archiveRef(index, "", children)
	if err != nil || got != repo+"@"+lay.Root.String() {
		t.Errorf("archiveRef(no ref) = %q, %v; want the index pinned", got, err)
	}
}

func TestCopyDigestRef(t *testing.T) {
	reg := copyRegistry(t)
	const repo = "registry.example.com/team/app"
	lay, _ := copyIndex(t, reg, repo, "1.0")
	out := filepath.Join(t.TempDir(), "app.tar")

	if err := copyArchive(context.Background(), io.Discard, out, []string{repo + "@" + lay.Root.String()}, copyAMD64); err != nil {
		t.Fatalf("copyArchive: %v", err)
	}
	entries := archiveEntries(t, readArchive(t, out))
	if len(entries) != 2 {
		t.Fatalf("index.json lists %d entries, want 2", len(entries))
	}
	checkEntry(t, entries[0], lay.Root, ocispec.MediaTypeImageIndex, repo+"@"+lay.Root.String())
}

func TestCopyManifestRoot(t *testing.T) {
	reg := copyRegistry(t)
	const repo = "registry.example.com/team/single"
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), copyAMD64, octest.File{Name: "etc/os", Mode: 0o644, Body: "single"})
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	path, err := ggcrlayout.FromPath(lay.Dir)
	if err != nil {
		t.Fatalf("layout.FromPath: %v", err)
	}
	hash, err := ggcrv1.NewHash(lay.Root.String())
	if err != nil {
		t.Fatalf("NewHash: %v", err)
	}
	img, err := path.Image(hash)
	if err != nil {
		t.Fatalf("layout.Image: %v", err)
	}
	tag, err := name.NewTag(repo + ":v1")
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if err := remote.Write(tag, img, remote.WithTransport(reg.Transport())); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	out := filepath.Join(t.TempDir(), "single.tar")

	if err := copyArchive(context.Background(), io.Discard, out, []string{repo + ":v1"}, copyAMD64); err != nil {
		t.Fatalf("copyArchive: %v", err)
	}
	entries := archiveEntries(t, readArchive(t, out))
	if len(entries) != 1 {
		t.Fatalf("index.json lists %d entries, want 1", len(entries))
	}
	checkEntry(t, entries[0], lay.Root, ocispec.MediaTypeImageManifest, repo+":v1")
}

func TestCopyTwoRefsShareBlobs(t *testing.T) {
	reg := copyRegistry(t)
	const repo = "registry.example.com/team/app"
	lay, _ := copyIndex(t, reg, repo, "1.0")
	idx, err := remote.Index(name.MustParseReference(repo+":1.0"), remote.WithTransport(reg.Transport()))
	if err != nil {
		t.Fatalf("remote.Index: %v", err)
	}
	if err := remote.WriteIndex(name.MustParseReference(repo+":2.0"), idx, remote.WithTransport(reg.Transport())); err != nil {
		t.Fatalf("tag 2.0: %v", err)
	}
	out := filepath.Join(t.TempDir(), "two.tar")

	if err := copyArchive(context.Background(), io.Discard, out, []string{repo + ":1.0", repo + ":2.0"}, copyAMD64); err != nil {
		t.Fatalf("copyArchive: %v", err)
	}
	entries := archiveEntries(t, readArchive(t, out))
	if len(entries) != 3 {
		t.Fatalf("index.json lists %d entries, want 3 (two tags and one platform manifest): %+v", len(entries), entries)
	}
	checkEntry(t, entries[0], lay.Root, ocispec.MediaTypeImageIndex, repo+":1.0")
	checkEntry(t, entries[1], lay.Manifest, ocispec.MediaTypeImageManifest, repo+"@"+lay.Manifest.String())
	checkEntry(t, entries[2], lay.Root, ocispec.MediaTypeImageIndex, repo+":2.0")
}

// noArchive checks that a failed copy left neither the archive nor a
// temporary file in its directory.
func noArchive(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range left {
		t.Errorf("failed copy left %s", e.Name())
	}
}

func TestCopyMissingPlatform(t *testing.T) {
	reg := copyRegistry(t)
	const repo = "registry.example.com/team/app"
	copyIndex(t, reg, repo, "1.0")
	dir := t.TempDir()

	err := copyArchive(context.Background(), io.Discard, filepath.Join(dir, "app.tar"), []string{repo + ":1.0"}, ocispec.Platform{OS: "linux", Architecture: "s390x"})
	if err == nil {
		t.Fatal("copyArchive of a platform the index lacks succeeded")
	}
	noArchive(t, dir)
}

// corruptBlob flips a byte of every response that serves the blob it names.
type corruptBlob struct {
	inner http.RoundTripper
	blob  digest.Digest
}

func (c corruptBlob) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(req)
	if err != nil || req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/blobs/"+c.blob.String()) {
		return resp, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	b[len(b)/2] ^= 0xff
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

func TestCopyCorruptLayer(t *testing.T) {
	reg := copyRegistry(t)
	const repo = "registry.example.com/team/app"
	lay, _ := copyIndex(t, reg, repo, "1.0")
	remote.DefaultTransport = corruptBlob{inner: reg.Transport(), blob: lay.Layers[1]}
	dir := t.TempDir()

	err := copyArchive(context.Background(), io.Discard, filepath.Join(dir, "app.tar"), []string{repo + ":1.0"}, copyAMD64)
	if err == nil || !strings.Contains(err.Error(), lay.Layers[1].Encoded()) {
		t.Fatalf("copyArchive of a corrupt layer: err = %v, want it to fail on that layer", err)
	}
	noArchive(t, dir)
}

func TestRunCopyArgValidation(t *testing.T) {
	out := filepath.Join(t.TempDir(), "app.tar")
	for _, args := range [][]string{
		{"copy", "registry.example.com/team/app:1.0"},
		{"copy", "-o", out},
		{"copy", "-o", out, "--platform", "linux", "registry.example.com/team/app:1.0"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) exit = %d, want 2 (usage); stderr: %s", args, code, stderr.String())
		}
	}
}
