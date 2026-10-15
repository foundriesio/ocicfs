// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package refstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	identity := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.SHA256.FromString("manifest"),
		Size:      123,
	}
	if err := s.Put(ref, resolved, identity, pulledAt, []string{"linux/arm64"}); err != nil {
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
	if got.Identity.Digest != identity.Digest || got.Identity.Size != identity.Size {
		t.Fatalf("identity = %#v", got.Identity)
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
	if err := s.Put(ref, first, first, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := s.Put(ref, second, second, time.Unix(1, 0).UTC(), []string{"linux/arm64"}); err != nil {
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
	identity := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.SHA256.FromString("manifest"),
		Size:      9,
	}
	pulledAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if err := s.Put(ref, resolved, identity, pulledAt, []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Ref != ref || got.Resolved.Digest != resolved.Digest || got.Resolved.Size != resolved.Size {
		t.Fatalf("record = %#v", got)
	}
	if got.Identity.Digest != identity.Digest || got.Identity.Size != identity.Size {
		t.Fatalf("record identity = %#v", got.Identity)
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
		if err := s.Put(ref, resolved, resolved, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
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
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromString("root")}
	if err := s.Put(ref, desc, desc, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
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
	desc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromString("resolved"),
		Size:      42,
	}
	if err := s.Put("app:v1", desc, desc, time.Unix(1720000000, 0).UTC(), []string{"linux/amd64"}); err != nil {
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
	desc := ocispec.Descriptor{Digest: digest.SHA256.FromString("root")}
	if err := s.Put(ref, desc, desc, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
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

// TestGetRejectsMissingOrMalformedIdentity: the identity descriptor is what
// every lookup keys on, so a record that omits it or names something that is
// not a sha256 digest is not a usable ref and is rejected on read.
func TestGetRejectsMissingOrMalformedIdentity(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name:    "absent",
			mutate:  func(doc map[string]any) { delete(doc, "identity") },
			wantErr: "identity digest",
		},
		{
			name: "empty digest",
			mutate: func(doc map[string]any) {
				doc["identity"].(map[string]any)["digest"] = ""
			},
			wantErr: "identity digest",
		},
		{
			name: "non-sha256",
			mutate: func(doc map[string]any) {
				doc["identity"].(map[string]any)["digest"] = "sha512:" + strings.Repeat("a", 128)
			},
			wantErr: "identity digest",
		},
		{
			name: "negative size",
			mutate: func(doc map[string]any) {
				doc["identity"].(map[string]any)["size"] = -1
			},
			wantErr: "identity size is negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			s, err := New(root)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ref := "app@sha256:abc"
			seedRecord(t, s, ref)
			rewriteRecord(t, root, ref, tt.mutate)

			if _, err := s.Get(ref); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Get error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestGetRejectsSupersededSchemaNamingTheRemedy: a record written by a version
// that keyed images differently cannot be read as if it meant the same thing,
// so Get fails closed and the error says what to do about it.
func TestGetRejectsSupersededSchemaNamingTheRemedy(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app@sha256:abc"
	seedRecord(t, s, ref)
	rewriteRecord(t, root, ref, func(doc map[string]any) {
		doc["schema"] = "application/vnd.ocicfs.ref.v1+json"
		delete(doc, "identity")
	})

	_, err = s.Get(ref)
	if err == nil {
		t.Fatal("Get: got nil, want a superseded-schema rejection")
	}
	for _, want := range []string{"v1+json", refSchema, "recreate the store"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Get error = %v, want it to contain %q", err, want)
		}
	}
}

// TestListSkipsSupersededSchemaRecord: one unreadable record must not brick the
// listing for every other image, so List skips it and warns with the same
// remedy Get reports.
func TestListSkipsSupersededSchemaRecord(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seedRecord(t, s, "healthy:v1")
	seedRecord(t, s, "stale:v1")
	rewriteRecord(t, root, "stale:v1", func(doc map[string]any) {
		doc["schema"] = "application/vnd.ocicfs.ref.v1+json"
		delete(doc, "identity")
	})

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].Ref != "healthy:v1" {
		t.Fatalf("records = %#v, want only the readable record", records)
	}
	if !strings.Contains(logged.String(), "recreate the store") {
		t.Fatalf("warning = %q, want it to name the remedy", logged.String())
	}
}

// seedRecord writes a valid record for ref whose two descriptors differ, so a
// test that corrupts one can tell which was rejected.
func seedRecord(t *testing.T, s *Store, ref string) {
	t.Helper()
	resolved := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.SHA256.FromString("index"),
		Size:      456,
	}
	identity := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.SHA256.FromString("manifest"),
		Size:      123,
	}
	if err := s.Put(ref, resolved, identity, time.Unix(0, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// rewriteRecord applies mutate to the stored JSON for ref, so a test can
// produce a record no Put would write.
func rewriteRecord(t *testing.T, root, ref string, mutate func(map[string]any)) {
	t.Helper()
	path := octestRefPath(root, ref)
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	mutate(doc)
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G306 G703 -- test rewrites a path it wrote
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestListStrictErrorsOnMalformedRecord pins the gc census's strictness: a
// record List would skip with a warning must fail ListStrict, because a
// skipped record silently unprotects its image from the sweep.
func TestListStrictErrorsOnMalformedRecord(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	desc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromString("resolved"),
		Size:      42,
	}
	if err := s.Put("app:v1", desc, desc, time.Unix(1720000000, 0).UTC(), []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	records, err := s.ListStrict(context.Background())
	if err != nil || len(records) != 1 || records[0].Ref != "app:v1" {
		t.Fatalf("ListStrict on healthy store = %v, %v, want the one record", records, err)
	}

	dir := filepath.Join(root, "refs", "sha256", "aa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, strings.Repeat("b", 62)+".json")
	if err := os.WriteFile(path, []byte(`{"schema":"wrong"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := s.ListStrict(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid ref record") {
		t.Fatalf("ListStrict = %v, want the invalid-record error", err)
	}
}

// TestPutHeldRoundTrip: a runtime reference records its holder and reads back
// with it; a plain Put records none.
func TestPutHeldRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.SHA256.FromString("manifest"), Size: 1}
	pulledAt := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if named, err := s.PutHeld("app:v1", desc, desc, pulledAt, []string{"linux/amd64"}, "cfs-build-1"); err != nil || !named {
		t.Fatalf("PutHeld = %v, %v; want the record installed", named, err)
	}
	if err := s.Put("app:v2", desc, desc, pulledAt, []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	held, err := s.Get("app:v1")
	if err != nil {
		t.Fatalf("Get held: %v", err)
	}
	if held.Holder != "cfs-build-1" {
		t.Fatalf("held record holder = %q, want cfs-build-1", held.Holder)
	}
	plain, err := s.Get("app:v2")
	if err != nil {
		t.Fatalf("Get plain: %v", err)
	}
	if plain.Holder != "" {
		t.Fatalf("plain record holder = %q, want none", plain.Holder)
	}
	if _, err := s.PutHeld("app:v3", desc, desc, pulledAt, []string{"linux/amd64"}, "Bad/Holder"); err == nil {
		t.Fatal("PutHeld accepted a holder name the hold store rejects")
	}
}

// TestPutHeldNeverReplacesAgentRecord: a runtime reference is not installed
// over an agent's record of the same reference, which stays as it was; it
// does replace an earlier runtime reference, and an agent's Put replaces it.
func TestPutHeldNeverReplacesAgentRecord(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	agent := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.SHA256.FromString("agent"), Size: 1}
	runtime := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.SHA256.FromString("runtime"), Size: 2}
	pulledAt := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if err := s.Put("app:v1", agent, agent, pulledAt, []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if named, err := s.PutHeld("app:v1", runtime, runtime, pulledAt, []string{"linux/amd64"}, "cfs-build-1"); err != nil || named {
		t.Fatalf("PutHeld over an agent's record = %v, %v; want not installed", named, err)
	}
	got, err := s.Get("app:v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Holder != "" || got.Identity.Digest != agent.Digest {
		t.Fatalf("record after a refused PutHeld = %#v, want the agent's untouched", got)
	}

	if named, err := s.PutHeld("app:v2", runtime, runtime, pulledAt, []string{"linux/amd64"}, "cfs-build-1"); err != nil || !named {
		t.Fatalf("PutHeld of a new name = %v, %v; want installed", named, err)
	}
	if named, err := s.PutHeld("app:v2", agent, agent, pulledAt, []string{"linux/amd64"}, "cfs-build-2"); err != nil || !named {
		t.Fatalf("PutHeld over a runtime record = %v, %v; want installed", named, err)
	}
	if got, err := s.Get("app:v2"); err != nil || got.Holder != "cfs-build-2" {
		t.Fatalf("record after a second PutHeld = %#v, %v; want the later runtime reference", got, err)
	}
	if err := s.Put("app:v2", agent, agent, pulledAt, []string{"linux/amd64"}); err != nil {
		t.Fatalf("Put over a runtime record: %v", err)
	}
	if got, err := s.Get("app:v2"); err != nil || got.Holder != "" {
		t.Fatalf("record after the agent's Put = %#v, %v; want the agent's", got, err)
	}
}
