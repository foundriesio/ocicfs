// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/octest"
	"github.com/foundriesio/knarr/internal/rootfs"
)

func TestParseDigestPinnedRef(t *testing.T) {
	hex256 := strings.Repeat("a", 64)
	want := digest.Digest("sha256:" + hex256)
	tests := []struct {
		name    string
		ref     string
		want    digest.Digest
		wantErr string
	}{
		{"name digest", "registry.example.com/team/app@" + want.String(), want, ""},
		{"bare digest", want.String(), want, ""},
		{"tag only", "registry.example.com/team/app:v1", "", "digest-pinned reference is required"},
		{"sha-prefixed name", "sha-registry.com/app:v1", "", "digest-pinned reference is required"},
		{"empty", "", "", "digest-pinned reference is required"},
		{"wrong algorithm", "sha512:" + strings.Repeat("b", 128), "", "non-sha256"},
		{"malformed", "sha256:bad", "", "invalid digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDigestPinnedRef(tt.ref)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDigestPinnedRef: %v", err)
			}
			if got != tt.want {
				t.Fatalf("digest = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPullWithoutFromDirReportsRegistryUnsupported(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = eng.Pull(context.Background(), "registry.example.com/team/app:latest")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatalf("error = %v, want registry unsupported, not ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), registryPullUnsupported) {
		t.Fatalf("error = %v, want registry unsupported", err)
	}
}

func TestPullFromDirE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
		octest.File{Name: "usr/bin/app", Mode: 0o755, Body: "#!/bin/sh\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	img, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir))
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Ref != lay.Ref {
		t.Fatalf("ref = %q, want %q", img.Ref, lay.Ref)
	}
	if img.Digest != lay.Root.String() {
		t.Fatalf("digest = %q, want %s", img.Digest, lay.Root)
	}
	if img.Size <= 0 {
		t.Fatalf("size = %d, want > 0", img.Size)
	}

	binding := readPullBinding(t, storeRoot, lay.Root, lay.Platform)
	if binding.RootFS.Descriptor.MediaType != rootfs.MediaTypeComposefsImage {
		t.Fatalf("composefs mediaType = %q", binding.RootFS.Descriptor.MediaType)
	}
	if binding.RootFS.Descriptor.Size != img.Size {
		t.Fatalf("binding size = %d, returned image size = %d", binding.RootFS.Descriptor.Size, img.Size)
	}
	if binding.Platform.OS != lay.Platform.OS ||
		binding.Platform.Architecture != lay.Platform.Architecture ||
		binding.Platform.Variant != lay.Platform.Variant {
		t.Fatalf("binding platform = %#v, want %#v", binding.Platform, lay.Platform)
	}
	if binding.RootFS.Fsverity.Mode != "required" || binding.RootFS.Fsverity.Source != "local-measurement" {
		t.Fatalf("binding fsverity = %#v", binding.RootFS.Fsverity)
	}
	assertObjectExists(t, storeRoot, binding.RootFS.Descriptor.Digest)

	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	measured, err := obj.VerityDigest(binding.RootFS.Descriptor.Digest)
	if err != nil {
		t.Fatalf("VerityDigest: %v", err)
	}
	if measured != binding.RootFS.Fsverity.Digest.String() {
		t.Fatalf("fs-verity digest = %q, binding records %s", measured, binding.RootFS.Fsverity.Digest)
	}

	assertObjectExists(t, storeRoot, lay.Root)
	assertObjectExists(t, storeRoot, lay.Manifest)
	assertObjectExists(t, storeRoot, lay.Config)
	assertObjectExists(t, storeRoot, lay.FileDigest)

	ref := readPullRef(t, storeRoot, lay.Ref)
	if ref.Ref != lay.Ref {
		t.Fatalf("stored ref = %q, want %q", ref.Ref, lay.Ref)
	}
	if ref.Resolved.Digest != lay.Root {
		t.Fatalf("resolved digest = %s, want %s", ref.Resolved.Digest, lay.Root)
	}
	if ref.Resolved.Size != lay.RootSize {
		t.Fatalf("resolved size = %d, want %d", ref.Resolved.Size, lay.RootSize)
	}
	if len(ref.Platforms) != 1 || ref.Platforms[0] != ocilayout.PlatformString(&lay.Platform) {
		t.Fatalf("platforms = %#v, want %s", ref.Platforms, ocilayout.PlatformString(&lay.Platform))
	}
}

type (
	pullBindingDoc struct {
		Platform ocispec.Platform `json:"platform"`
		RootFS   struct {
			Descriptor struct {
				MediaType string        `json:"mediaType"`
				Digest    digest.Digest `json:"digest"`
				Size      int64         `json:"size"`
			} `json:"descriptor"`
			Fsverity struct {
				Mode   string        `json:"mode"`
				Digest digest.Digest `json:"digest"`
				Source string        `json:"source"`
			} `json:"fsverity"`
		} `json:"rootfs"`
	}

	pullRefDoc struct {
		Ref      string `json:"ref"`
		Resolved struct {
			MediaType string        `json:"mediaType"`
			Digest    digest.Digest `json:"digest"`
			Size      int64         `json:"size"`
		} `json:"resolved"`
		Platforms []string `json:"platforms"`
	}
)

func readPullBinding(t *testing.T, root string, d digest.Digest, p ocispec.Platform) pullBindingDoc {
	t.Helper()
	data, err := os.ReadFile(octest.BindingPath(root, d, p)) // #nosec G304 -- test reads a path derived from the digest it wrote
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	var got pullBindingDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal binding: %v", err)
	}
	return got
}

func readPullRef(t *testing.T, root, ref string) pullRefDoc {
	t.Helper()
	data, err := os.ReadFile(octest.RefPath(root, ref)) // #nosec G304 -- test reads a path derived from the ref it wrote
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	var got pullRefDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal ref: %v", err)
	}
	return got
}

func assertObjectExists(t *testing.T, root string, d digest.Digest) {
	t.Helper()
	if _, err := os.Stat(octest.ObjectPath(root, d)); err != nil {
		t.Fatalf("object %s not present: %v", d, err)
	}
}
