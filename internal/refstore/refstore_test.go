// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package refstore

import (
	"encoding/json"
	"os"
	"path/filepath"
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
