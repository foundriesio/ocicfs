// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package refstore persists reference provenance: a record of the exact image
// reference a pull was given and the immutable descriptor it resolved to. It is
// auxiliary UX/provenance metadata, not a trust anchor and not required to run
// an image -- a digest-pinned image is found via its resolved digest in the
// object store; refs exist for listing, debugging, and naming. Being untrusted,
// a record is checked on use: its stored ref must equal the reference looked up,
// its refKey must equal sha256(ref), and its stored digests must be valid
// sha256 digests. Each record is a JSON file keyed by the SHA-256 of the
// reference string exactly as given to Put -- refstore stores and compares
// refs verbatim and never canonicalizes them itself; the engine normalizes a
// ref before it reaches Put or Get, so equivalent refs (e.g. "app:v1" and
// "docker.io/library/app:v1") converge on the same key by the time refstore
// sees them:
//
//	refKey = sha256(ref)
//	<root>/refs/sha256/<refkey-xx>/<refkey-rest>.json
//
// The record holds the exact ref, its refKey, the resolved OCI root descriptor
// (whose digest is the image identity, locating its content in the object store),
// the pull time, and the platforms pulled. The file is written atomically and
// durably.
//
//	{ "schema": "application/vnd.knarr.ref.v1+json",
//	  "ref": "registry.example.com/team/app@sha256:...",
//	  "refKey":   {"algorithm":"sha256","digest":"sha256:..."},
//	  "resolved": {"mediaType":"...","digest":"sha256:...","size":N},
//	  "pulledAt": "<RFC3339>", "platforms": ["linux/arm64"] }
package refstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/durable"
)

// Store writes reference provenance under the knarr store root.
type Store struct {
	root string
}

// Record is the validated reference record stored for a pulled image.
type Record struct {
	Ref       string
	Resolved  ocispec.Descriptor
	PulledAt  time.Time
	Platforms []string
}

// ErrNotFound reports that no reference record exists for an exact ref string.
var ErrNotFound = errors.New("refstore: record not found")

// New opens a reference store rooted at root.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("refstore: empty store root")
	}
	return &Store{root: root}, nil
}

// Put writes refs/sha256/<refkey-xx>/<refkey-rest>.json atomically.
func (s *Store) Put(ref string, resolved ocispec.Descriptor, pulledAt time.Time, platforms []string) error {
	path, refKey := s.refPath(ref)
	doc := refDoc{
		Schema: refSchema,
		Ref:    ref,
		RefKey: refKeyDoc{
			Algorithm: "sha256",
			Digest:    refKey,
		},
		Resolved:  descriptorDoc{MediaType: resolved.MediaType, Digest: resolved.Digest, Size: resolved.Size},
		PulledAt:  pulledAt.Format(time.RFC3339),
		Platforms: platforms,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("refstore: marshal ref: %w", err)
	}
	data = append(data, '\n')

	if err := durable.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("refstore: create ref dir: %w", err)
	}
	if err := durable.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("refstore: write ref: %w", err)
	}
	return nil
}

// List returns all validated reference records sorted by exact ref string. An
// invalid record is not a usable ref: it is skipped with a warning rather than
// failing the whole listing, so one corrupt file cannot brick every other
// image's ls and remove.
func (s *Store) List() ([]Record, error) {
	base := filepath.Join(s.root, "refs", "sha256")
	var records []Record
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("refstore: access %s: %w", path, walkErr)
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rec, err := readRecord(path, "")
		if errors.Is(err, ErrNotFound) {
			// The file vanished between the walk and the read.
			return nil
		}
		if err != nil {
			slog.Warn("refstore: skipping invalid ref record", "path", path, "err", err)
			return nil
		}
		records = append(records, rec)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("refstore: list refs: %w", err)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Ref < records[j].Ref
	})
	return records, nil
}

// Get reads the validated record for an exact ref string.
func (s *Store) Get(ref string) (Record, error) {
	path, _ := s.refPath(ref)
	rec, err := readRecord(path, ref)
	if errors.Is(err, ErrNotFound) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("refstore: read ref: %w", err)
	}
	return rec, nil
}

// Delete removes the record for an exact ref string.
func (s *Store) Delete(ref string) error {
	path, _ := s.refPath(ref)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("refstore: delete ref: %w", err)
	}
	return nil
}

const refSchema = "application/vnd.knarr.ref.v1+json"

// The on-disk ref JSON documents.
type (
	refDoc struct {
		Schema    string        `json:"schema"`
		Ref       string        `json:"ref"`
		RefKey    refKeyDoc     `json:"refKey"`
		Resolved  descriptorDoc `json:"resolved"`
		PulledAt  string        `json:"pulledAt"`
		Platforms []string      `json:"platforms"`
	}

	refKeyDoc struct {
		Algorithm string        `json:"algorithm"`
		Digest    digest.Digest `json:"digest"`
	}

	descriptorDoc struct {
		MediaType string        `json:"mediaType"`
		Digest    digest.Digest `json:"digest"`
		Size      int64         `json:"size"`
	}
)

func (s *Store) refPath(ref string) (string, digest.Digest) {
	refKey := key(ref)
	hexKey := refKey.Encoded()
	return filepath.Join(s.root, "refs", "sha256", hexKey[:2], hexKey[2:]+".json"), refKey
}

func readRecord(path, wantRef string) (Record, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- ref-key-derived store path
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read: %w", err)
	}
	var doc refDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Record{}, fmt.Errorf("decode: %w", err)
	}
	return validateRecord(doc, wantRef)
}

func validateRecord(doc refDoc, wantRef string) (Record, error) {
	if doc.Schema != refSchema {
		return Record{}, fmt.Errorf("unsupported ref schema %q", doc.Schema)
	}
	if doc.Ref == "" {
		return Record{}, errors.New("empty ref")
	}
	if wantRef != "" && doc.Ref != wantRef {
		return Record{}, fmt.Errorf("ref mismatch: got %q, want %q", doc.Ref, wantRef)
	}
	if doc.RefKey.Algorithm != "sha256" {
		return Record{}, fmt.Errorf("unsupported ref key algorithm %q", doc.RefKey.Algorithm)
	}
	if err := digestutil.ValidateSHA256(doc.RefKey.Digest); err != nil {
		return Record{}, fmt.Errorf("ref key digest: %w", err)
	}
	if doc.RefKey.Digest != key(doc.Ref) {
		return Record{}, fmt.Errorf("ref key mismatch: got %s, want %s", doc.RefKey.Digest, key(doc.Ref))
	}
	if err := digestutil.ValidateSHA256(doc.Resolved.Digest); err != nil {
		return Record{}, fmt.Errorf("resolved digest: %w", err)
	}
	if doc.Resolved.Size < 0 {
		return Record{}, fmt.Errorf("resolved size is negative: %d", doc.Resolved.Size)
	}
	pulledAt, err := time.Parse(time.RFC3339, doc.PulledAt)
	if err != nil {
		return Record{}, fmt.Errorf("pulledAt: %w", err)
	}
	if len(doc.Platforms) == 0 {
		return Record{}, errors.New("no platforms")
	}
	for _, platform := range doc.Platforms {
		if platform == "" {
			return Record{}, errors.New("empty platform")
		}
	}
	return Record{
		Ref: doc.Ref,
		Resolved: ocispec.Descriptor{
			MediaType: doc.Resolved.MediaType,
			Digest:    doc.Resolved.Digest,
			Size:      doc.Resolved.Size,
		},
		PulledAt:  pulledAt,
		Platforms: append([]string(nil), doc.Platforms...),
	}, nil
}

func key(ref string) digest.Digest {
	sum := sha256.Sum256([]byte(ref))
	return digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:]))
}
