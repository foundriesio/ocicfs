// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocilayout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/ocichain"
)

type fixture struct {
	t   *testing.T
	dir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	return &fixture{t: t, dir: dir}
}

func (f *fixture) writeBlob(data []byte, mediaType string) ocispec.Descriptor {
	f.t.Helper()
	d := digest.SHA256.FromBytes(data)
	p := filepath.Join(f.dir, "blobs", "sha256", d.Hex())
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatalf("write blob: %v", err)
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("expected error for empty directory")
	}
}

func TestSourceOpenReadsBlob(t *testing.T) {
	f := newFixture(t)
	desc := f.writeBlob([]byte("content"), ocispec.MediaTypeImageLayer)
	src := openSource(t, f.dir)

	rc, err := src.Open(context.Background(), desc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer closeReader(t, rc)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, []byte("content")) {
		t.Fatalf("content = %q, want content", got)
	}
}

func TestSourceOpenRejectsNonSHA256(t *testing.T) {
	src := openSource(t, t.TempDir())
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.Digest("sha512:" + strings.Repeat("a", 128)),
		Size:      4,
	}
	if _, err := src.Open(context.Background(), desc); err == nil || !strings.Contains(err.Error(), "non-sha256") {
		t.Fatalf("error = %v, want non-sha256 rejection", err)
	}
}

func TestSourceOpenSymlinkSourceRejected(t *testing.T) {
	f := newFixture(t)
	desc := f.writeBlob([]byte("content"), ocispec.MediaTypeImageLayer)
	p := filepath.Join(f.dir, "blobs", "sha256", desc.Digest.Hex())
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	if err := os.Symlink("/etc/hostname", p); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	src := openSource(t, f.dir)
	_, err := src.Open(context.Background(), desc)
	if err == nil {
		t.Fatal("expected rejection of symlink source")
	}
	if !strings.Contains(err.Error(), "open blob") {
		t.Fatalf("error = %v, want symlink rejected at open", err)
	}
}

func TestSourceOpenFIFOSourceRejected(t *testing.T) {
	f := newFixture(t)
	desc := f.writeBlob([]byte("content"), ocispec.MediaTypeImageLayer)
	p := filepath.Join(f.dir, "blobs", "sha256", desc.Digest.Hex())
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	src := openSource(t, f.dir)
	_, err := src.Open(context.Background(), desc)
	if err == nil {
		t.Fatal("expected rejection of FIFO source")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("error = %v, want regular-file rejection", err)
	}
}

func TestSourceOpenCtxCancelledBeforeOpen(t *testing.T) {
	f := newFixture(t)
	desc := f.writeBlob([]byte("content"), ocispec.MediaTypeImageLayer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := openSource(t, f.dir).Open(ctx, desc); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSourceOpenRangeServesSection(t *testing.T) {
	f := newFixture(t)
	blob := []byte("0123456789abcdefghij")
	desc := f.writeBlob(blob, ocispec.MediaTypeImageLayer)
	src := openSource(t, f.dir).(ocichain.RangeSource)

	for _, r := range []struct{ off, n int64 }{{3, 5}, {0, 20}, {19, 1}, {7, 0}, {20, 0}} {
		rc, whole, err := src.OpenRange(context.Background(), desc, r.off, r.n)
		if err != nil {
			t.Fatalf("OpenRange(%d, %d): %v", r.off, r.n, err)
		}
		if whole {
			t.Errorf("OpenRange(%d, %d) reported whole", r.off, r.n)
		}
		got, err := io.ReadAll(rc)
		closeReader(t, rc)
		if err != nil {
			t.Fatalf("read range (%d, %d): %v", r.off, r.n, err)
		}
		if want := blob[r.off : r.off+r.n]; !bytes.Equal(got, want) {
			t.Errorf("OpenRange(%d, %d) = %q, want %q", r.off, r.n, got, want)
		}
	}
}

func TestSourceOpenRangeRejectsOutOfBounds(t *testing.T) {
	f := newFixture(t)
	desc := f.writeBlob([]byte("0123456789"), ocispec.MediaTypeImageLayer)
	src := openSource(t, f.dir).(ocichain.RangeSource)

	for _, r := range []struct{ off, n int64 }{{8, 3}, {11, 0}, {0, 11}, {-1, 2}, {2, -1}, {1, 1<<63 - 1}} {
		if _, _, err := src.OpenRange(context.Background(), desc, r.off, r.n); err == nil {
			t.Errorf("OpenRange(%d, %d) = nil error, want rejection", r.off, r.n)
		}
	}

	// The descriptor bounds the range, not the file: a file longer than its
	// descriptor does not serve bytes past the descriptor's size, and a
	// descriptor longer than its file does not serve bytes the file lacks.
	short := desc
	short.Size = 4
	if _, _, err := src.OpenRange(context.Background(), short, 5, 1); err == nil {
		t.Error("OpenRange past the descriptor size succeeded")
	}
	long := desc
	long.Size = 20
	if _, _, err := src.OpenRange(context.Background(), long, 15, 1); err == nil {
		t.Error("OpenRange past the file end succeeded")
	}
	negative := desc
	negative.Size = -1
	if _, _, err := src.OpenRange(context.Background(), negative, 0, 0); err == nil {
		t.Error("OpenRange of a negative-size descriptor succeeded")
	}
}

func TestSourceOpenRangeCtxCancelled(t *testing.T) {
	f := newFixture(t)
	desc := f.writeBlob([]byte("0123456789"), ocispec.MediaTypeImageLayer)
	src := openSource(t, f.dir).(ocichain.RangeSource)

	ctx, cancel := context.WithCancel(context.Background())
	rc, _, err := src.OpenRange(ctx, desc, 2, 6)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer closeReader(t, rc)
	cancel()
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context.Canceled", err)
	}
	if _, _, err := src.OpenRange(ctx, desc, 2, 6); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenRange error = %v, want context.Canceled at open", err)
	}
}

// TestResolveMissingRootListsIndex covers the common single-platform-export
// case where the pinned digest is absent but index.json points at a platform
// manifest present in the layout.
func TestResolveMissingRootListsIndex(t *testing.T) {
	f := newFixture(t)
	layers := []ocispec.Descriptor{fakeLayerDesc("a")}
	cfg := f.writeBlob(configJSON("linux", "amd64", "", diffIDsFor(layers)), ocispec.MediaTypeImageConfig)
	man := f.writeBlob(manifestJSON(cfg, layers), ocispec.MediaTypeImageManifest)
	man.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}

	if err := os.WriteFile(filepath.Join(f.dir, "index.json"), indexJSON([]ocispec.Descriptor{man}), 0o644); err != nil {
		t.Fatal(err)
	}

	missing := digest.SHA256.FromString("dropped-index-digest")
	_, err := ocichain.Resolve(context.Background(), openSource(t, f.dir), missing, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err == nil {
		t.Fatal("expected error for a root digest absent from the layout")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not present in the layout") {
		t.Errorf("error should explain the digest is absent: %v", err)
	}
	if !strings.Contains(msg, man.Digest.String()) {
		t.Errorf("error should list the manifest in index.json (%s): %v", man.Digest, err)
	}
	if !strings.Contains(msg, "linux/amd64") {
		t.Errorf("error should annotate the listed manifest's platform: %v", err)
	}
}

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want ocispec.Platform
	}{
		{
			name: "os arch",
			in:   "linux/amd64",
			want: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		},
		{
			name: "variant",
			in:   "linux/arm/v7",
			want: ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v7"},
		},
		{
			name: "normalized",
			in:   "Linux/ARM64/V8",
			want: ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePlatform(tt.in)
			if err != nil {
				t.Fatalf("ParsePlatform: %v", err)
			}
			if got.OS != tt.want.OS || got.Architecture != tt.want.Architecture || got.Variant != tt.want.Variant {
				t.Fatalf("platform = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParsePlatformRejectsInvalidStrings(t *testing.T) {
	for _, in := range []string{"", "linux", "linux/", "linux/amd64/v8/extra", "linux/amd64/v8.1"} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePlatform(in); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func openSource(t *testing.T, dir string) ocichain.BlobSource {
	t.Helper()
	src, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return src
}

func closeReader(t *testing.T, rc io.Closer) {
	t.Helper()
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func fakeLayerDesc(content string) ocispec.Descriptor {
	data := []byte(content)
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerZstd,
		Digest:    digest.SHA256.FromBytes(data),
		Size:      int64(len(data)),
	}
}

func configJSON(osName, arch, variant string, diffIDs []string) []byte {
	m := map[string]any{
		"architecture": arch,
		"os":           osName,
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": diffIDs,
		},
	}
	if variant != "" {
		m["variant"] = variant
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func manifestJSON(config ocispec.Descriptor, layers []ocispec.Descriptor) []byte {
	man := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    layers,
	}
	b, err := json.Marshal(man)
	if err != nil {
		panic(err)
	}
	return b
}

func indexJSON(entries []ocispec.Descriptor) []byte {
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: entries,
	}
	b, err := json.Marshal(idx)
	if err != nil {
		panic(err)
	}
	return b
}

func diffIDsFor(layers []ocispec.Descriptor) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = digest.SHA256.FromString("diff-" + l.Digest.Hex()).String()
	}
	return out
}
