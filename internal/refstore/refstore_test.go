// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package refstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPutWritesRef(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "registry.example.com/team/app@sha256:abc"
	refKey := key(ref)
	resolved := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.SHA256.FromString("root"),
		Size:      456,
	}
	pulledAt := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	if err := s.Put(ref, resolved, pulledAt, []string{"linux/arm64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	path := filepath.Join(root, "refs", "sha256", refKey.Encoded()[:2], refKey.Encoded()[2:]+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got refDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Schema != refSchema {
		t.Fatalf("schema = %q", got.Schema)
	}
	if got.Ref != ref {
		t.Fatalf("ref = %q, want exact input %q", got.Ref, ref)
	}
	if got.RefKey.Algorithm != "sha256" || got.RefKey.Digest != refKey {
		t.Fatalf("refKey = %#v, want %s", got.RefKey, refKey)
	}
	if got.Resolved.Digest != resolved.Digest || got.Resolved.Size != resolved.Size {
		t.Fatalf("resolved = %#v", got.Resolved)
	}
	if got.PulledAt != "2026-06-26T12:00:00Z" {
		t.Fatalf("pulledAt = %q", got.PulledAt)
	}
	if len(got.Platforms) != 1 || got.Platforms[0] != "linux/arm64" {
		t.Fatalf("platforms = %#v", got.Platforms)
	}
}

func TestPutPreservesExactRefForKey(t *testing.T) {
	a := key("app:v1")
	b := key("docker.io/library/app:v1")
	if a == b {
		t.Fatal("distinct exact refs produced the same key")
	}
}

func TestPutReplacesAtomically(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app:v1"
	first := ocispec.Descriptor{Digest: digest.SHA256.FromString("first"), Size: 1}
	second := ocispec.Descriptor{Digest: digest.SHA256.FromString("second"), Size: 2}
	if err := s.Put(ref, first, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := s.Put(ref, second, time.Unix(1, 0).UTC(), []string{"linux/arm64"}); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	refKey := key(ref)
	data, err := os.ReadFile(filepath.Join(root, "refs", "sha256", refKey.Encoded()[:2], refKey.Encoded()[2:]+".json")) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got refDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Resolved.Digest != second.Digest || got.PulledAt != "1970-01-01T00:00:01Z" {
		t.Fatalf("ref was not replaced: %#v", got)
	}
}

func TestGetRoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app@sha256:abc"
	resolved := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.SHA256.FromString("root"),
		Size:      12,
	}
	pulledAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if err := s.Put(ref, resolved, pulledAt, []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Ref != ref || got.Resolved.Digest != resolved.Digest || got.Resolved.Size != resolved.Size {
		t.Fatalf("record = %#v", got)
	}
	if !got.PulledAt.Equal(pulledAt) {
		t.Fatalf("pulledAt = %s, want %s", got.PulledAt, pulledAt)
	}
	if len(got.Platforms) != 1 || got.Platforms[0] != "linux/amd64" {
		t.Fatalf("platforms = %#v", got.Platforms)
	}
}

func TestListReturnsSortedRecords(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolved := ocispec.Descriptor{Digest: digest.SHA256.FromString("root"), Size: 1}
	for _, ref := range []string{"z@sha256:def", "a@sha256:abc"} {
		if err := s.Put(ref, resolved, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
			t.Fatalf("Put %s: %v", ref, err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Ref != "a@sha256:abc" || got[1].Ref != "z@sha256:def" {
		t.Fatalf("records not sorted: %#v", got)
	}
}

func TestListEmptyStore(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List len = %d, want empty", len(got))
	}
}

func TestDeleteThenGetReturnsErrNotFound(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app@sha256:abc"
	if err := s.Put(ref, ocispec.Descriptor{Digest: digest.SHA256.FromString("root")}, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
}

func TestListSkipsMalformedRecord(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Put("app:v1", ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromString("resolved"),
		Size:      42,
	}, time.Unix(1720000000, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	dir := filepath.Join(root, "refs", "sha256", "aa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, strings.Repeat("b", 62)+".json")
	if err := os.WriteFile(path, []byte(`{"schema":"wrong"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].Ref != "app:v1" {
		t.Fatalf("records = %#v, want only the valid app:v1 record", records)
	}
}

func TestGetRejectsInvalidRecord(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app@sha256:abc"
	if err := s.Put(ref, ocispec.Descriptor{Digest: digest.SHA256.FromString("root")}, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data, err := os.ReadFile(octestRefPath(root, ref))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data = []byte(strings.Replace(string(data), `"digest":"sha256:`, `"digest":"sha512:`, 1))
	if err := os.WriteFile(octestRefPath(root, ref), data, 0o644); err != nil { // #nosec G703 -- test rewrites a path it wrote
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := s.Get(ref); err == nil {
		t.Fatal("expected invalid digest error")
	}
}

func octestRefPath(root, ref string) string {
	refKey := key(ref)
	return filepath.Join(root, "refs", "sha256", refKey.Encoded()[:2], refKey.Encoded()[2:]+".json")
}
