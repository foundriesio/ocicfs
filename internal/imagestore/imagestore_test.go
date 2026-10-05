// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package imagestore

import (
	"context"
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
		Canonical:      true,
		// Put must write the current scheme's tuple, not whatever a caller puts here.
		ObjectStorePath: "objects/bogus",
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
	if got.RootFS.ObjectStore.Type != "composefs-digest-store" ||
		got.RootFS.ObjectStore.Algorithm != "fsverity-sha256-12" ||
		got.RootFS.ObjectStore.Path != "objects/fsverity-sha256-12" {
		t.Fatalf("object store = %#v", got.RootFS.ObjectStore)
	}
	if !got.RootFS.Canonical {
		t.Fatalf("canonical = %v, want true", got.RootFS.Canonical)
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
	if !binding.Canonical {
		t.Fatalf("Get canonical = %v, want true", binding.Canonical)
	}
	if binding.ObjectStorePath != "objects/fsverity-sha256-12" {
		t.Fatalf("Get object store path = %q", binding.ObjectStorePath)
	}
	if binding.RootfsSize != 456 || got.RootFS.Size != 456 {
		t.Fatalf("rootfs size = %d (doc %d), want 456", binding.RootfsSize, got.RootFS.Size)
	}
}

// TestGetRejectsOldObjectStoreTuple asserts a binding carrying the superseded
// sha256 object-store tuple is rejected as a cache miss (ErrNotFound), so the
// caller rebuilds it rather than resolving its redirects through the wrong view.
func TestGetRejectsOldObjectStoreTuple(t *testing.T) {
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
	data = []byte(strings.NewReplacer(
		`"algorithm":"fsverity-sha256-12"`, `"algorithm":"sha256"`,
		`"path":"objects/fsverity-sha256-12"`, `"path":"objects/sha256"`,
	).Replace(string(data)))
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G703 -- test rewrites a path it wrote
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := s.Get(root, platform); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
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

// TestPutSerializesCanonicalFalse pins that the canonical flag is written even
// in its default false state: a reader must see an explicit value, not infer
// one from an omitted key.
func TestPutSerializesCanonicalFalse(t *testing.T) {
	root := digest.SHA256.FromString("root-canonical-false")
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "amd64"}, Binding{
		Descriptor:     ocispec.Descriptor{MediaType: "application/vnd.knarr.composefs.image.v1+erofs", Digest: digest.SHA256.FromString("cfs"), Size: 1},
		FsverityDigest: digest.SHA256.FromString("fsv").String(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := filepath.Join(s.root, "images", "sha256", root.Hex()[:2], root.Hex()[2:], "linux-amd64.json")
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `"canonical":false`) {
		t.Fatalf("binding JSON must carry an explicit canonical:false, got:\n%s", data)
	}
}

func validTestBinding(cfs, fsv digest.Digest) Binding {
	return Binding{
		Descriptor: ocispec.Descriptor{
			MediaType: "application/vnd.knarr.composefs.image.v1+erofs",
			Digest:    cfs,
			Size:      123,
		},
		FsverityDigest: fsv.String(),
	}
}

func TestPlatformsListsBindings(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := digest.SHA256.FromString("platforms root")
	b := validTestBinding(digest.SHA256.FromString("cfs"), digest.SHA256.FromString("fsv"))
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "amd64"}, b); err != nil {
		t.Fatalf("Put amd64: %v", err)
	}
	// The default variant canonicalizes away, in the filename and the document.
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, b); err != nil {
		t.Fatalf("Put arm64/v8: %v", err)
	}

	got, err := s.Platforms(context.Background(), root)
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	want := map[string]bool{"linux/amd64": true, "linux/arm64": true}
	names := make([]string, len(got))
	for i, p := range got {
		names[i] = p.OS + "/" + p.Architecture
		if p.Variant != "" {
			names[i] += "/" + p.Variant
		}
	}
	if len(names) != 2 || !want[names[0]] || !want[names[1]] || names[0] == names[1] {
		t.Fatalf("Platforms = %v, want linux/amd64 and linux/arm64", names)
	}

	none, err := s.Platforms(context.Background(), digest.SHA256.FromString("no bindings"))
	if err != nil || len(none) != 0 {
		t.Fatalf("Platforms of absent root = %v, %v, want none", none, err)
	}
}

// TestPlatformsRejectsCorruptBinding pins the strictness gc's mark phase
// relies on: a binding that cannot be decoded must fail the listing, not
// vanish from it, because a vanished binding would sweep a wanted image.
func TestPlatformsRejectsCorruptBinding(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := digest.SHA256.FromString("corrupt root")
	b := validTestBinding(digest.SHA256.FromString("cfs"), digest.SHA256.FromString("fsv"))
	if err := s.Put(root, ocispec.Platform{OS: "linux", Architecture: "amd64"}, b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	dir := filepath.Join(s.root, "images", "sha256", root.Hex()[:2], root.Hex()[2:])
	if err := os.WriteFile(filepath.Join(dir, "linux-arm64.json"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := s.Platforms(context.Background(), root); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("Platforms = %v, want a decode error", err)
	}

	// A binding whose recorded platform does not name its file is a mismatch,
	// not a listing entry.
	if err := os.WriteFile(filepath.Join(dir, "linux-arm64.json"), []byte(`{"schema":"application/vnd.knarr.cfs-binding.v1+json","platform":{"os":"linux","architecture":"riscv64"}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := s.Platforms(context.Background(), root); err == nil || !strings.Contains(err.Error(), "names linux-riscv64.json") {
		t.Fatalf("Platforms = %v, want a filename-mismatch error", err)
	}
}

func TestWalkListsBindingsAndSkipsForeign(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b := validTestBinding(digest.SHA256.FromString("cfs"), digest.SHA256.FromString("fsv"))
	first := digest.SHA256.FromString("walk first")
	second := digest.SHA256.FromString("walk second")
	if err := s.Put(first, ocispec.Platform{OS: "linux", Architecture: "amd64"}, b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(second, ocispec.Platform{OS: "linux", Architecture: "arm64"}, b); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Foreign entries are skipped, not fatal: the walk feeds deletion, and a
	// file it cannot key simply is not deleted.
	if err := os.WriteFile(filepath.Join(s.root, "images", "sha256", "stray.json"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := map[string]bool{}
	err = s.Walk(context.Background(), func(root digest.Digest, p ocispec.Platform) error {
		got[root.String()+" "+p.OS+"/"+p.Architecture] = true
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 2 ||
		!got[first.String()+" linux/amd64"] ||
		!got[second.String()+" linux/arm64"] {
		t.Fatalf("Walk = %#v, want both bindings", got)
	}
}

func TestSavedLayerPutGetDelete(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blob := digest.SHA256.FromString("layer blob")
	if _, err := s.GetLayer(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetLayer before PutLayer = %v, want os.ErrNotExist", err)
	}
	if err := s.DeleteLayer(blob); err != nil {
		t.Fatalf("DeleteLayer of an absent layer: %v", err)
	}

	if err := s.PutLayer(blob, []byte("first")); err != nil {
		t.Fatalf("PutLayer: %v", err)
	}
	path := filepath.Join(s.root, "layers", "sha256", blob.Hex()[:2], blob.Hex()[2:])
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved layer mode = %v, want 0600", fi.Mode().Perm())
	}
	if err := s.PutLayer(blob, []byte("second")); err != nil {
		t.Fatalf("PutLayer over an existing layer: %v", err)
	}
	got, err := s.GetLayer(blob)
	if err != nil {
		t.Fatalf("GetLayer: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("GetLayer = %q, want the bytes last put", got)
	}

	if err := s.DeleteLayer(blob); err != nil {
		t.Fatalf("DeleteLayer: %v", err)
	}
	if _, err := s.GetLayer(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetLayer after DeleteLayer = %v, want os.ErrNotExist", err)
	}
	if err := s.PruneEmptyDirs(); err != nil {
		t.Fatalf("PruneEmptyDirs: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shard directory after PruneEmptyDirs: %v, want it gone", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "layers", "sha256")); err != nil {
		t.Fatalf("layers root after PruneEmptyDirs: %v", err)
	}
}

func TestDeleteLayerAbsentIsOK(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blob := digest.SHA256.FromString("layer blob")
	if err := s.DeleteLayer(blob); err != nil {
		t.Fatalf("DeleteLayer with no layers tree: %v", err)
	}
	if err := s.PutLayer(blob, []byte("x")); err != nil {
		t.Fatalf("PutLayer: %v", err)
	}
	if err := s.DeleteLayer(blob); err != nil {
		t.Fatalf("DeleteLayer: %v", err)
	}
	if err := s.DeleteLayer(blob); err != nil {
		t.Fatalf("DeleteLayer of an absent layer in a present shard: %v", err)
	}
	if err := os.Remove(filepath.Join(s.root, "layers", "sha256", blob.Hex()[:2])); err != nil {
		t.Fatalf("remove shard: %v", err)
	}
	if err := s.DeleteLayer(blob); err != nil {
		t.Fatalf("DeleteLayer with the shard directory absent: %v", err)
	}
}

func TestSavedLayerRejectsInvalidDigest(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, blob := range []digest.Digest{"", "sha256:../../etc/passwd", digest.Digest("sha512:" + strings.Repeat("a", 128))} {
		if err := s.PutLayer(blob, []byte("x")); err == nil {
			t.Errorf("PutLayer(%q) succeeded", blob)
		}
		if _, err := s.GetLayer(blob); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("GetLayer(%q) = %v, want a digest error", blob, err)
		}
		if err := s.DeleteLayer(blob); err == nil {
			t.Errorf("DeleteLayer(%q) succeeded", blob)
		}
	}
}

func TestWalkLayersListsSavedLayersAndSkipsForeign(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.WalkLayers(context.Background(), func(digest.Digest) error {
		t.Fatal("WalkLayers called fn on a store with no saved layer")
		return nil
	}); err != nil {
		t.Fatalf("WalkLayers on a store with no saved layer: %v", err)
	}

	first := digest.SHA256.FromString("walk first")
	second := digest.SHA256.FromString("walk second")
	for _, blob := range []digest.Digest{first, second} {
		if err := s.PutLayer(blob, []byte("x")); err != nil {
			t.Fatalf("PutLayer: %v", err)
		}
	}
	// Foreign entries are skipped, not fatal: the walk feeds deletion, and a
	// file it cannot key simply is not deleted.
	shard := filepath.Join(s.root, "layers", "sha256", first.Hex()[:2])
	for _, stray := range []string{filepath.Join(s.root, "layers", "sha256", "stray"), filepath.Join(shard, ".knarr-123")} {
		if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	got := map[digest.Digest]bool{}
	err = s.WalkLayers(context.Background(), func(blob digest.Digest) error {
		got[blob] = true
		return nil
	})
	if err != nil {
		t.Fatalf("WalkLayers: %v", err)
	}
	if len(got) != 2 || !got[first] || !got[second] {
		t.Fatalf("WalkLayers = %v, want both saved layers", got)
	}
}

// TestSavedLayerSizeLimit asserts a saved layer over the size limit is neither
// written nor read: a file that large under layers/ is refused, not loaded.
func TestSavedLayerSizeLimit(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blob := digest.SHA256.FromString("layer blob")
	if err := s.PutLayer(blob, make([]byte, maxLayerBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("PutLayer over the limit = %v, want the size limit", err)
	}
	if _, err := s.GetLayer(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetLayer after the refused PutLayer = %v, want os.ErrNotExist", err)
	}

	if err := s.PutLayer(blob, []byte("small")); err != nil {
		t.Fatalf("PutLayer: %v", err)
	}
	path := filepath.Join(s.root, "layers", "sha256", blob.Hex()[:2], blob.Hex()[2:])
	if err := os.Truncate(path, maxLayerBytes); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if data, err := s.GetLayer(blob); err != nil || len(data) != maxLayerBytes {
		t.Fatalf("GetLayer at the limit = %d bytes, %v", len(data), err)
	}
	if err := os.Truncate(path, maxLayerBytes+1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := s.GetLayer(blob); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("GetLayer over the limit = %v, want the size limit", err)
	}
}
