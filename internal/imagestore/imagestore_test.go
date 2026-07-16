// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package imagestore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPutWritesBinding(t *testing.T) {
	root := digest.SHA256.FromString("root")
	cfs := digest.SHA256.FromString("composefs")
	fsv := digest.SHA256.FromString("verity")
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, Binding{
		Descriptor: ocispec.Descriptor{
			MediaType: "application/vnd.knarr.composefs.image.v1+erofs",
			Digest:    cfs,
			Size:      123,
		},
		FsverityDigest: fsv.String(),
		RootfsSize:     456,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// arm64/v8 (the default arm64 variant) is canonicalized to a variant-less key.
	path := filepath.Join(s.root, "images", "sha256", root.Hex()[:2], root.Hex()[2:], "linux-arm64.json")
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got bindingDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Schema != bindingSchema {
		t.Fatalf("schema = %q", got.Schema)
	}
	if got.Platform != (platformDoc{OS: "linux", Architecture: "arm64"}) {
		t.Fatalf("platform = %#v", got.Platform)
	}
	if got.RootFS.Type != "composefs" || got.RootFS.Descriptor.Digest != cfs || got.RootFS.Descriptor.Size != 123 {
		t.Fatalf("rootfs descriptor = %#v", got.RootFS)
	}
	if got.RootFS.ObjectStore.Path != "objects/sha256" {
		t.Fatalf("object store path = %q", got.RootFS.ObjectStore.Path)
	}
	if got.RootFS.Fsverity.Mode != "required" || got.RootFS.Fsverity.Digest != fsv || got.RootFS.Fsverity.Source != "local-measurement" {
		t.Fatalf("fsverity = %#v", got.RootFS.Fsverity)
	}

	binding, err := s.Get(root, ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if binding.Descriptor.Digest != cfs || binding.Descriptor.Size != 123 || binding.FsverityDigest != fsv.String() {
		t.Fatalf("Get = %#v", binding)
	}
	if binding.RootfsSize != 456 || got.RootFS.Size != 456 {
		t.Fatalf("rootfs size = %d (doc %d), want 456", binding.RootfsSize, got.RootFS.Size)
	}
}

// TestGetLegacyBindingWithoutRootfsSize asserts a binding written before the
// size field existed still reads cleanly, reporting the size as unknown (0),
// and that a zero size is omitted from the JSON on write.
func TestGetLegacyBindingWithoutRootfsSize(t *testing.T) {
	root := digest.SHA256.FromString("root")
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(root, platform, Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs")},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	path, _, err := s.bindingPath(root, platform)
	if err != nil {
		t.Fatalf("bindingPath: %v", err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc struct {
		RootFS map[string]json.RawMessage `json:"rootfs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := doc.RootFS["size"]; ok {
		t.Fatalf("zero rootfs size not omitted from binding JSON:\n%s", data)
	}

	binding, err := s.Get(root, platform)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if binding.RootfsSize != 0 {
		t.Fatalf("legacy binding rootfs size = %d, want 0 (unknown)", binding.RootfsSize)
	}
}

func TestPlatformPathCanonicalizesDefaultVariant(t *testing.T) {
	// An image config may name the architecture's default variant explicitly
	// (linux/arm64/v8) while a host lookup is variant-less (linux/arm64); both
	// must key to the same binding, else a pulled image cannot be found.
	_, withVariant, err := platformPath(ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatalf("platformPath arm64/v8: %v", err)
	}
	_, plain, err := platformPath(ocispec.Platform{OS: "linux", Architecture: "arm64"})
	if err != nil {
		t.Fatalf("platformPath arm64: %v", err)
	}
	if withVariant != "linux-arm64" || plain != "linux-arm64" {
		t.Fatalf("platformPath: arm64/v8=%q arm64=%q, want both linux-arm64", withVariant, plain)
	}
}

func TestPutOmitsEmptyVariant(t *testing.T) {
	root := digest.SHA256.FromString("root")
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "amd64"}, Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs")},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := filepath.Join(s.root, "images", "sha256", root.Hex()[:2], root.Hex()[2:], "linux-amd64.json")
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	platform := raw["platform"].(map[string]any)
	if _, ok := platform["variant"]; ok {
		t.Fatalf("variant should be omitted: %s", data)
	}
}

func TestPutRejectsInvalidInputs(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	valid := Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs")},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}
	tests := []struct {
		name string
		root digest.Digest
		p    ocispec.Platform
		b    Binding
	}{
		{"root algorithm", digest.Digest("sha512:abc"), ocispec.Platform{OS: "linux", Architecture: "amd64"}, valid},
		{"bad os", digest.SHA256.FromString("root"), ocispec.Platform{OS: "linux/../x", Architecture: "amd64"}, valid},
		{"bad arch", digest.SHA256.FromString("root"), ocispec.Platform{OS: "linux", Architecture: "amd64.v2"}, valid},
		{"empty fsverity", digest.SHA256.FromString("root"), ocispec.Platform{OS: "linux", Architecture: "amd64"}, Binding{Descriptor: valid.Descriptor}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.Put(tt.root, tt.p, tt.b); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = s.Get(digest.SHA256.FromString("root"), ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestDeleteRemovesBinding(t *testing.T) {
	root := digest.SHA256.FromString("root")
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(root, platform, Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs")},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(root, platform); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(root, platform); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestDeleteMissingIsOK(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Delete(digest.SHA256.FromString("root"), ocispec.Platform{OS: "linux", Architecture: "amd64"}); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestGetRejectsInvalidBinding(t *testing.T) {
	root := digest.SHA256.FromString("root")
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "amd64"}, Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs")},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	path, _, err := s.bindingPath(root, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("bindingPath: %v", err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data = []byte(strings.Replace(string(data), `"digest":"sha256:`, `"digest":"sha512:`, 1))
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G703 -- test rewrites a path it wrote
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := s.Get(root, ocispec.Platform{OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatal("expected error")
	}
}
