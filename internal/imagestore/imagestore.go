// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package imagestore persists the prepared-image binding: a local index mapping
// {image identity, platform} to the composefs image already built for it, so a
// mount, after verifying the stored metadata chain, can use that image
// directly instead of rebuilding it. The identity is the digest of the platform image manifest, the
// one key an image is stored under: a reference landing on an image index is
// descended to that manifest before a binding is written or read, so an image
// pulled by its index and by its manifest shares one binding. It is auxiliary,
// not authoritative -- the object store alone
// holds the image's verified OCI metadata and content, and a re-pull regenerates
// a lost binding without re-storing them, so the binding is a convenience (and
// the on-disk record of the locally-measured fs-verity digest). Being untrusted,
// it must be checked on use: what it names is verified against the object store
// -- the composefs image is content-addressed there, and at mount its measured
// fs-verity digest must match the identity manifest's sealing annotation when
// one is present, the binding's recorded digest otherwise -- so a wrong or
// stale binding cannot cause unverified content to be mounted. Each binding is
// a small JSON file keyed by that identity digest and its platform:
//
//	<root>/images/sha256/<root-xx>/<root-rest>/<os>[-<arch>[-<variant>]].json
//
// It records the composefs blob descriptor, the object store backing it
// (objects/fsverity-sha256-12, the fs-verity view the image's redirects resolve
// through), the locally measured fs-verity digest -- always mode "required"
// with a "local-measurement" source; the digest a mount enforces for an image
// whose identity manifest carries no sealing annotation -- whether the merged
// image was built by the
// upstream-reproducible construction (the canonical flag), and the merged
// rootfs's apparent size as display metadata (optional; absent in bindings
// written before it existed). The file is written atomically and durably.
//
//	{ "schema": "application/vnd.ocicfs.cfs-binding.v1+json",
//	  "platform": {"os":"linux","architecture":"arm64","variant":"v8"},
//	  "rootfs": { "type":"composefs",
//	    "descriptor":  {"mediaType":"...composefs.image.v1+erofs","digest":"sha256:...","size":N},
//	    "objectStore": {"type":"composefs-digest-store","algorithm":"fsverity-sha256-12","path":"objects/fsverity-sha256-12"},
//	    "fsverity":    {"mode":"required","digest":"sha256:...","source":"local-measurement"},
//	    "canonical":   false,
//	    "size":        N } }
//
// Beside the bindings the store keeps saved layers: the file list a scan of a
// layer's tar stream produced, kept so a later pull naming the same layer
// blob can merge the layer without reading it. Each is one file named by the
// digest of the compressed layer blob, holding whatever bytes the caller
// encoded:
//
//	<root>/layers/sha256/<blob-xx>/<blob-rest>
//
// A saved layer is auxiliary like a binding: losing one costs the next pull a
// read of that layer and nothing else. It is owner-only, since a file list
// carries the bytes of the layer's smallest files.
package imagestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/digestutil"
	"github.com/foundriesio/ocicfs/internal/durable"
)

// Store writes prepared-image bindings under the ocicfs store root.
type Store struct {
	root string
}

// Binding is the rootfs data recorded in a prepared-image binding.
// RootfsSize is the apparent size of the merged rootfs measured when the
// image was built (regular-file content bytes, hardlink groups counted
// once); it is display metadata, and 0 when the binding predates the field
// (recorded when the image is next actually built — a pull that verifies
// the image as already present does not rewrite the binding).
// Canonical reports whether the merged image was built by the
// upstream-reproducible construction; it is persisted so a cached pull can
// apply the same decision after restart.
// ObjectStorePath is the store-relative directory the mount's data plane reads.
// It is populated by Get from the recorded tuple; Put always writes the current
// scheme's path and ignores this field.
type Binding struct {
	Descriptor      ocispec.Descriptor
	FsverityDigest  string
	RootfsSize      int64
	Canonical       bool
	ObjectStorePath string
}

// ErrNotFound reports that no prepared-image binding exists for a root digest
// and platform.
var ErrNotFound = errors.New("imagestore: binding not found")

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
				Type:      objectStoreType,
				Algorithm: objectStoreAlgorithm,
				Path:      objectStorePath,
			},
			Fsverity: fsverityDoc{
				Mode:   "required",
				Digest: fsv,
				Source: "local-measurement",
			},
			Canonical: b.Canonical,
			Size:      b.RootfsSize,
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
		FsverityDigest:  doc.RootFS.Fsverity.Digest.String(),
		RootfsSize:      doc.RootFS.Size,
		Canonical:       doc.RootFS.Canonical,
		ObjectStorePath: doc.RootFS.ObjectStore.Path,
	}, nil
}

// Delete removes images/sha256/<root-xx>/<root-rest>/<platform>.json.
func (s *Store) Delete(root digest.Digest, p ocispec.Platform) error {
	path, _, err := s.bindingPath(root, p)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("imagestore: delete binding: %w", err)
	}
	return nil
}

// PruneEmptyDirs removes the empty directories binding and saved-layer
// removal leave behind: each image's own directory and the shard level above
// it, and the saved layers' shard level. Call it only under the
// store-exclusive gc lock.
func (s *Store) PruneEmptyDirs() error {
	if err := durable.PruneEmptyDirs(filepath.Join(s.root, "images", "sha256"), 2); err != nil {
		return fmt.Errorf("imagestore: %w", err)
	}
	if err := durable.PruneEmptyDirs(filepath.Join(s.root, "layers", "sha256"), 1); err != nil {
		return fmt.Errorf("imagestore: %w", err)
	}
	return nil
}

// Platforms returns the platforms that have a binding on disk for root, each
// read from the binding document itself and required to agree with its
// filename. It is strict: gc marks an image's wanted objects through every
// binding this returns, so a binding file that does not decode, or whose
// recorded platform does not name the file it sits in, is an error rather
// than a skip — skipped, its composefs image and objects would be swept while
// the identity is still wanted. A root with no binding directory has no
// platforms and returns nil.
func (s *Store) Platforms(ctx context.Context, root digest.Digest) ([]ocispec.Platform, error) {
	if err := digestutil.ValidateSHA256(root); err != nil {
		return nil, fmt.Errorf("imagestore: root digest: %w", err)
	}
	dir := filepath.Join(s.root, "images", "sha256", root.Hex()[:2], root.Hex()[2:])
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("imagestore: read bindings of %s: %w", root, err)
	}
	platforms := make([]ocispec.Platform, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := bindingPlatform(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			return nil, fmt.Errorf("imagestore: binding of %s: %w", root, err)
		}
		platforms = append(platforms, p)
	}
	return platforms, nil
}

// Walk calls fn for every binding on disk as its (root, platform) key. Unlike
// Platforms it is lenient: the walk feeds the orphan-binding sweep, where a
// malformed entry that cannot be keyed simply is not deleted, so it is
// skipped with a warning rather than failing the whole walk.
func (s *Store) Walk(ctx context.Context, fn func(root digest.Digest, p ocispec.Platform) error) error {
	base := filepath.Join(s.root, "images", "sha256")
	err := filepath.WalkDir(base, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("imagestore: walk bindings: %w", walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return fmt.Errorf("imagestore: walk bindings: %w", err)
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 || len(parts[0]) != 2 {
			slog.Warn("imagestore: skipping foreign entry under images", "path", path)
			return nil
		}
		root := digest.NewDigestFromEncoded(digest.SHA256, parts[0]+parts[1])
		if digestutil.ValidateSHA256(root) != nil {
			slog.Warn("imagestore: skipping foreign entry under images", "path", path)
			return nil
		}
		p, err := bindingPlatform(path, parts[2])
		if err != nil {
			slog.Warn("imagestore: skipping unreadable binding", "path", path, "err", err)
			return nil
		}
		return fn(root, p)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// PutLayer writes data as the saved layer of the layer blob named blob, at
// layers/sha256/<blob-xx>/<blob-rest>, atomically and durably, replacing one
// already there. Data larger than GetLayer reads is refused.
func (s *Store) PutLayer(blob digest.Digest, data []byte) error {
	path, err := s.layerPath(blob)
	if err != nil {
		return err
	}
	if len(data) > maxLayerBytes {
		return fmt.Errorf("imagestore: saved layer of %d bytes exceeds the limit of %d", len(data), maxLayerBytes)
	}
	if err := durable.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("imagestore: create saved layer dir: %w", err)
	}
	if err := durable.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("imagestore: write saved layer: %w", err)
	}
	return nil
}

// GetLayer reads the saved layer of the layer blob named blob. An absent one
// surfaces as an error satisfying errors.Is(err, os.ErrNotExist); a file
// larger than a saved layer may be is refused rather than read into memory.
func (s *Store) GetLayer(blob digest.Digest) ([]byte, error) {
	path, err := s.layerPath(blob)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) // #nosec G304 -- digest-derived saved layer path
	if err != nil {
		return nil, fmt.Errorf("imagestore: read saved layer: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("imagestore: failed to close saved layer", "path", path, "err", err)
		}
	}()
	// One byte past the cap tells a larger file from one that ends there.
	data, err := io.ReadAll(io.LimitReader(f, maxLayerBytes+1))
	if err != nil {
		return nil, fmt.Errorf("imagestore: read saved layer: %w", err)
	}
	if len(data) > maxLayerBytes {
		return nil, fmt.Errorf("imagestore: saved layer %s exceeds the limit of %d bytes", blob, maxLayerBytes)
	}
	return data, nil
}

// DeleteLayer removes the saved layer of the layer blob named blob, durably:
// a saved layer removed because it broke a pull must not come back after a
// crash to break the next. An absent one, its shard directory included, is
// not an error.
func (s *Store) DeleteLayer(blob digest.Digest) error {
	path, err := s.layerPath(blob)
	if err != nil {
		return err
	}
	if err := durable.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("imagestore: delete saved layer: %w", err)
	}
	return nil
}

// WalkLayers calls fn for every saved layer on disk with the digest of the
// layer blob it is named by. It is lenient as Walk is: the walk feeds a sweep,
// where an entry that cannot be keyed simply is not deleted, so it is skipped
// with a warning rather than failing the whole walk.
func (s *Store) WalkLayers(ctx context.Context, fn func(blob digest.Digest) error) error {
	base := filepath.Join(s.root, "layers", "sha256")
	err := filepath.WalkDir(base, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("imagestore: walk saved layers: %w", walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return fmt.Errorf("imagestore: walk saved layers: %w", err)
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 || len(parts[0]) != 2 {
			slog.Warn("imagestore: skipping foreign entry under layers", "path", path)
			return nil
		}
		blob := digest.NewDigestFromEncoded(digest.SHA256, parts[0]+parts[1])
		if digestutil.ValidateSHA256(blob) != nil {
			slog.Warn("imagestore: skipping foreign entry under layers", "path", path)
			return nil
		}
		return fn(blob)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// bindingPlatform reads the platform a binding file records and requires it
// to name the file it sits in.
func bindingPlatform(path, name string) (ocispec.Platform, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- store-owned binding path
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("read %s: %w", name, err)
	}
	var doc bindingDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return ocispec.Platform{}, fmt.Errorf("decode %s: %w", name, err)
	}
	p := ocispec.Platform{OS: doc.Platform.OS, Architecture: doc.Platform.Architecture, Variant: doc.Platform.Variant}
	_, filename, err := platformPath(p)
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("platform of %s: %w", name, err)
	}
	if filename+".json" != name {
		return ocispec.Platform{}, fmt.Errorf("binding %s records platform %q, which names %s.json", name, doc.Platform, filename)
	}
	return p, nil
}

const (
	bindingSchema = "application/vnd.ocicfs.cfs-binding.v1+json"

	// The object-store tuple is the binding's compatibility key: a binding
	// carrying any other tuple is rejected as a cache miss, so the schema string
	// need not change when the tuple does.
	objectStoreType      = "composefs-digest-store"
	objectStoreAlgorithm = "fsverity-sha256-12"
	objectStorePath      = "objects/fsverity-sha256-12"

	// maxLayerBytes caps a saved layer file, written or read.
	maxLayerBytes = 64 << 20
)

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
		// Canonical records whether the merged image was built by the
		// upstream-reproducible construction. Always written; absent (false) in
		// bindings from an object-store scheme that predates it, which the tuple
		// check rejects as a cache miss anyway.
		Canonical bool `json:"canonical"`
		// Size is the merged rootfs's apparent size: display metadata, not
		// part of the trust chain, absent in bindings written before it
		// existed.
		Size int64 `json:"size,omitempty"`
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

// layerPath returns the path of the saved layer named by the layer blob
// digest blob.
func (s *Store) layerPath(blob digest.Digest) (string, error) {
	if err := digestutil.ValidateSHA256(blob); err != nil {
		return "", fmt.Errorf("imagestore: layer blob digest: %w", err)
	}
	return filepath.Join(s.root, "layers", "sha256", blob.Hex()[:2], blob.Hex()[2:]), nil
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
	if doc.RootFS.ObjectStore.Type != objectStoreType ||
		doc.RootFS.ObjectStore.Algorithm != objectStoreAlgorithm ||
		doc.RootFS.ObjectStore.Path != objectStorePath {
		// A binding written under a superseded object-store scheme (or any other
		// tuple) is a cache miss: the caller rebuilds it rather than reusing a
		// binding whose redirects resolve through a different view. Joining
		// ErrNotFound routes it through the same "run ocicfs pull" guidance a
		// missing binding gets.
		return fmt.Errorf("imagestore: unsupported object store %#v: %w", doc.RootFS.ObjectStore, ErrNotFound)
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
	if doc.RootFS.Size < 0 {
		return fmt.Errorf("imagestore: negative rootfs size %d", doc.RootFS.Size)
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
