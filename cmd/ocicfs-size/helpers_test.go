// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/pkg/version"
)

// tent is one tar entry of a test layer. typ defaults to a regular file.
type tent struct {
	name   string
	typ    byte
	body   string
	link   string
	mode   int64
	uid    int
	mtime  int64
	xattrs map[string]string
}

var testMtime = time.Unix(1700000000, 0)

func file(name, body string) tent { return tent{name: name, body: body} }
func dir(name string) tent        { return tent{name: name, typ: tar.TypeDir} }
func symlink(name, target string) tent {
	return tent{name: name, typ: tar.TypeSymlink, link: target}
}
func hardlink(name, target string) tent { return tent{name: name, typ: tar.TypeLink, link: target} }
func whiteout(name string) tent {
	d, b := filepath.Split(name)
	return file(d+".wh."+b, "")
}

func (e tent) header() *tar.Header {
	h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: e.mode, Uid: e.uid, Gid: e.uid, ModTime: testMtime, Format: tar.FormatPAX}
	if e.typ == 0 {
		h.Typeflag = tar.TypeReg
		h.Size = int64(len(e.body))
	}
	if h.Mode == 0 {
		h.Mode = 0o644
		if e.typ == tar.TypeDir {
			h.Mode = 0o755
		}
	}
	if e.mtime != 0 {
		h.ModTime = time.Unix(e.mtime, 0)
	}
	if len(e.xattrs) > 0 {
		h.PAXRecords = map[string]string{}
		for k, v := range e.xattrs {
			h.PAXRecords["SCHILY.xattr."+k] = v
		}
	}
	return h
}

// tarOf returns a tar stream of entries.
func tarOf(t testing.TB, entries ...tent) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(e.header()); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testImage is an image of a test layout: raw layer tars and how to
// compress them.
type testImage struct {
	layers    [][]byte
	mediaType string   // default plain tar
	layerType []string // per layer, overriding mediaType where set
	label     string   // a config label, to vary the config alone
}

// writeLayout writes images into an OCI layout under dir and returns a
// reference to each, "<dir>@<manifest digest>".
func writeLayout(t testing.TB, dir string, images ...testImage) []string {
	t.Helper()
	put := func(data []byte, mt string) ocispec.Descriptor {
		d := digest.FromBytes(data)
		p := filepath.Join(dir, "blobs", "sha256", d.Encoded())
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return ocispec.Descriptor{MediaType: mt, Digest: d, Size: int64(len(data))}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	var refs []string
	for _, img := range images {
		mt := img.mediaType
		if mt == "" {
			mt = ocispec.MediaTypeImageLayer
		}
		var descs []ocispec.Descriptor
		var diffIDs []digest.Digest
		for i, raw := range img.layers {
			mt := mt
			if i < len(img.layerType) && img.layerType[i] != "" {
				mt = img.layerType[i]
			}
			descs = append(descs, put(compress(t, raw, mt), mt))
			diffIDs = append(diffIDs, digest.FromBytes(raw))
		}
		cfg := ocispec.Image{Platform: platform, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: diffIDs}}
		if img.label != "" {
			cfg.Config.Labels = map[string]string{"l": img.label}
		}
		cfgJSON, _ := json.Marshal(cfg)
		m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: put(cfgJSON, ocispec.MediaTypeImageConfig), Layers: descs}
		mJSON, _ := json.Marshal(m)
		md := put(mJSON, ocispec.MediaTypeImageManifest)
		refs = append(refs, dir+"@"+md.Digest.String())
	}
	return refs
}

func compress(t testing.TB, raw []byte, mt string) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch mt {
	case ocispec.MediaTypeImageLayerGzip:
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(raw)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	case ocispec.MediaTypeImageLayerZstd:
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = zw.Write(raw)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		return raw
	}
	return buf.Bytes()
}

// spyOpener opens "<dir>@<digest>" references from OCI layouts and counts
// the blobs and the ranges read from them.
type spyOpener struct {
	mu     sync.Mutex
	opens  map[digest.Digest]int
	ranges map[digest.Digest]int
}

func newSpyOpener() *spyOpener {
	return &spyOpener{opens: map[digest.Digest]int{}, ranges: map[digest.Digest]int{}}
}

func (s *spyOpener) open(_ context.Context, ref string) (ocichain.BlobSource, digest.Digest, error) {
	i := strings.LastIndex(ref, "@")
	src, err := ocilayout.Open(ref[:i])
	if err != nil {
		return nil, "", err
	}
	return spySource{s: s, src: src}, digest.Digest(ref[i+1:]), nil
}

type spySource struct {
	s   *spyOpener
	src ocichain.BlobSource
}

func (ss spySource) Open(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ss.s.mu.Lock()
	ss.s.opens[desc.Digest]++
	ss.s.mu.Unlock()
	return ss.src.Open(ctx, desc)
}

func (ss spySource) OpenRange(ctx context.Context, desc ocispec.Descriptor, off, n int64) (io.ReadCloser, bool, error) {
	ss.s.mu.Lock()
	ss.s.ranges[desc.Digest]++
	ss.s.mu.Unlock()
	return ss.src.(ocichain.RangeSource).OpenRange(ctx, desc, off, n)
}

// layerInv scans a raw layer tar.
func layerInv(t testing.TB, raw []byte) *LayerInv {
	t.Helper()
	inv, err := scanLayer(context.Background(), io.NopCloser(bytes.NewReader(raw)), digest.FromBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// rootsOf replays images given as lists of raw layer tars.
func rootsOf(t testing.TB, images ...[][]byte) []*root {
	t.Helper()
	var roots []*root
	for n, layers := range images {
		img := &Image{Ref: "img" + string(rune('A'+n))}
		r := &root{img: img}
		var chain digest.Digest
		for _, raw := range layers {
			d := digest.FromBytes(raw)
			chain = chainID(chain, d)
			img.Layers = append(img.Layers, LayerRef{DiffID: d.String(), ChainID: chain.String()})
			r.mats = append(r.mats, materialize(layerInv(t, raw)))
		}
		r.view = replay(r.mats)
		roots = append(roots, r)
	}
	return roots
}

// stampVersion makes the binary look like a clean stamped build.
func stampVersion(t *testing.T, ver, rev string) {
	t.Helper()
	oldV, oldR := version.Version, version.Revision
	version.Version, version.Revision = ver, rev
	t.Cleanup(func() { version.Version, version.Revision = oldV, oldR })
}
