// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package imagestore persists the prepared-image binding: a local index mapping
// {OCI root digest, platform} to the composefs image already built for it, so a
// mount can use that image directly instead of re-resolving the OCI chain and
// rebuilding it. It is auxiliary, not authoritative -- the object store alone
// holds the image's verified OCI metadata and content, and a re-pull regenerates
// a lost binding without re-storing them, so the binding is a convenience (and
// the on-disk record of the locally-measured fs-verity digest). Being untrusted,
// it must be checked on use: what it names is verified against the object store
// -- the composefs image is content-addressed there and its fs-verity digest is
// enforced at mount -- so a wrong or stale binding cannot cause unverified
// content to be mounted. Each binding is
// a small JSON file keyed by the image's OCI root digest and its platform:
//
//	<root>/images/sha256/<root-xx>/<root-rest>/<os>[-<arch>[-<variant>]].json
//
// It records the composefs blob descriptor, the object store backing it
// (objects/sha256), and the fs-verity digest the mount enforces -- always mode
// "required" with a "local-measurement" source. The file is written atomically
// and durably.
//
//	{ "schema": "application/vnd.knarr.cfs-binding.v1+json",
//	  "platform": {"os":"linux","architecture":"arm64","variant":"v8"},
//	  "rootfs": { "type":"composefs",
//	    "descriptor":  {"mediaType":"...composefs.image.v1+erofs","digest":"sha256:...","size":N},
//	    "objectStore": {"type":"composefs-digest-store","algorithm":"sha256","path":"objects/sha256"},
//	    "fsverity":    {"mode":"required","digest":"sha256:...","source":"local-measurement"} } }
package imagestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/durable"
)

const (
	bindingSchema = "application/vnd.knarr.cfs-binding.v1+json"
)

// ErrNotFound reports that no prepared-image binding exists for a root digest
// and platform.
var ErrNotFound = errors.New("imagestore: binding not found")

// Store writes prepared-image bindings under the knarr store root.
type Store struct {
	root string
}

// Binding is the rootfs data recorded in a prepared-image binding.
type Binding struct {
	Descriptor     ocispec.Descriptor
	FsverityDigest string
}

// New opens an image binding store rooted at root.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("imagestore: empty store root")
	}
	return &Store{root: root}, nil
}

// Put writes images/sha256/<root-xx>/<root-rest>/<platform>.json atomically.
func (s *Store) Put(root digest.Digest, p ocispec.Platform, b Binding) error {
	fsv, err := digestutil.ParseSHA256(b.FsverityDigest)
	if err != nil {
		return fmt.Errorf("imagestore: fs-verity digest: %w", err)
	}
	path, platform, err := s.bindingPath(root, p)
	if err != nil {
		return err
	}

	doc := bindingDoc{
		Schema:   bindingSchema,
		Platform: platform,
		RootFS: rootFSDoc{
			Type:       "composefs",
			Descriptor: descriptorDoc{MediaType: b.Descriptor.MediaType, Digest: b.Descriptor.Digest, Size: b.Descriptor.Size},
			ObjectStore: objectStoreDoc{
				Type:      "composefs-digest-store",
				Algorithm: "sha256",
				Path:      "objects/sha256",
			},
			Fsverity: fsverityDoc{
				Mode:   "required",
				Digest: fsv,
				Source: "local-measurement",
			},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("imagestore: marshal binding: %w", err)
	}
	data = append(data, '\n')

	if err := durable.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("imagestore: create binding dir: %w", err)
	}
	if err := durable.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("imagestore: write binding: %w", err)
	}
	return nil
}

// Get reads images/sha256/<root-xx>/<root-rest>/<platform>.json.
func (s *Store) Get(root digest.Digest, p ocispec.Platform) (Binding, error) {
	path, platform, err := s.bindingPath(root, p)
	if err != nil {
		return Binding{}, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- digest- and platform-derived binding path
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, fmt.Errorf("imagestore: read binding: %w", err)
	}

	var doc bindingDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Binding{}, fmt.Errorf("imagestore: decode binding: %w", err)
	}
	if err := validateBinding(doc, platform); err != nil {
		return Binding{}, err
	}
	return Binding{
		Descriptor: ocispec.Descriptor{
			MediaType: doc.RootFS.Descriptor.MediaType,
			Digest:    doc.RootFS.Descriptor.Digest,
			Size:      doc.RootFS.Descriptor.Size,
		},
		FsverityDigest: doc.RootFS.Fsverity.Digest.String(),
	}, nil
}

// The on-disk binding JSON documents.
type (
	bindingDoc struct {
		Schema   string      `json:"schema"`
		Platform platformDoc `json:"platform"`
		RootFS   rootFSDoc   `json:"rootfs"`
	}

	platformDoc struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	}

	rootFSDoc struct {
		Type        string         `json:"type"`
		Descriptor  descriptorDoc  `json:"descriptor"`
		ObjectStore objectStoreDoc `json:"objectStore"`
		Fsverity    fsverityDoc    `json:"fsverity"`
	}

	descriptorDoc struct {
		MediaType string        `json:"mediaType"`
		Digest    digest.Digest `json:"digest"`
		Size      int64         `json:"size"`
	}

	objectStoreDoc struct {
		Type      string `json:"type"`
		Algorithm string `json:"algorithm"`
		Path      string `json:"path"`
	}

	fsverityDoc struct {
		Mode   string        `json:"mode"`
		Digest digest.Digest `json:"digest"`
		Source string        `json:"source"`
	}
)

func (s *Store) bindingPath(root digest.Digest, p ocispec.Platform) (string, platformDoc, error) {
	if err := digestutil.ValidateSHA256(root); err != nil {
		return "", platformDoc{}, fmt.Errorf("imagestore: root digest: %w", err)
	}
	platform, filename, err := platformPath(p)
	if err != nil {
		return "", platformDoc{}, err
	}
	path := filepath.Join(s.root, "images", "sha256", root.Hex()[:2], root.Hex()[2:], filename+".json")
	return path, platform, nil
}

func validateBinding(doc bindingDoc, wantPlatform platformDoc) error {
	if doc.Schema != bindingSchema {
		return fmt.Errorf("imagestore: unsupported binding schema %q", doc.Schema)
	}
	if doc.Platform != wantPlatform {
		return fmt.Errorf("imagestore: binding platform mismatch: got %#v, want %#v", doc.Platform, wantPlatform)
	}
	if doc.RootFS.Type != "composefs" {
		return fmt.Errorf("imagestore: unsupported rootfs type %q", doc.RootFS.Type)
	}
	if doc.RootFS.ObjectStore.Type != "composefs-digest-store" ||
		doc.RootFS.ObjectStore.Algorithm != "sha256" ||
		doc.RootFS.ObjectStore.Path != "objects/sha256" {
		return fmt.Errorf("imagestore: unsupported object store %#v", doc.RootFS.ObjectStore)
	}
	if doc.RootFS.Fsverity.Mode != "required" || doc.RootFS.Fsverity.Source != "local-measurement" {
		return fmt.Errorf("imagestore: unsupported fs-verity policy %#v", doc.RootFS.Fsverity)
	}
	if err := digestutil.ValidateSHA256(doc.RootFS.Descriptor.Digest); err != nil {
		return fmt.Errorf("imagestore: descriptor digest: %w", err)
	}
	if err := digestutil.ValidateSHA256(doc.RootFS.Fsverity.Digest); err != nil {
		return fmt.Errorf("imagestore: fs-verity digest: %w", err)
	}
	return nil
}

func platformPath(p ocispec.Platform) (platformDoc, string, error) {
	arch := strings.ToLower(p.Architecture)
	variant := strings.ToLower(p.Variant)
	// Canonicalize the architecture's default variant to empty: runtime.GOARCH
	// yields no variant, while an image config may name the default explicitly
	// (e.g. arm64/v8). Treating them as one key makes a host's linux/arm64 and
	// an image's linux/arm64/v8 resolve to the same binding.
	if variant == defaultVariant(arch) {
		variant = ""
	}
	parts := []string{strings.ToLower(p.OS), arch}
	if variant != "" {
		parts = append(parts, variant)
	}
	for _, part := range parts {
		if !validComponent(part) {
			return platformDoc{}, "", fmt.Errorf("imagestore: invalid platform component %q", part)
		}
	}
	doc := platformDoc{OS: parts[0], Architecture: arch}
	if variant != "" {
		doc.Variant = variant
	}
	return doc, strings.Join(parts, "-"), nil
}

// defaultVariant returns the conventional default CPU variant for an
// architecture, so a variant naming it is treated the same as none.
func defaultVariant(arch string) string {
	switch arch {
	case "arm64":
		return "v8"
	case "arm":
		return "v7"
	default:
		return ""
	}
}

func validComponent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
