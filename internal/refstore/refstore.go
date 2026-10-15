// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package refstore persists reference provenance: a record of the exact image
// reference a pull was given, the immutable descriptor it resolved to, and the
// identity of the image that descriptor leads to. It is auxiliary UX/provenance
// metadata, not a trust anchor and not required to run an image -- a
// digest-pinned image is found via its identity digest in the object store;
// refs exist for listing, debugging, and naming. Being untrusted,
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
// The record holds the exact ref, its refKey, two descriptors, the pull time,
// the platforms pulled and, for a runtime reference, the holder it names the
// image for. The file is written atomically and durably.
//
//	{ "schema": "application/vnd.ocicfs.ref.v2+json",
//	  "ref": "registry.example.com/team/app@sha256:...",
//	  "refKey":   {"algorithm":"sha256","digest":"sha256:..."},
//	  "resolved": {"mediaType":"...","digest":"sha256:...","size":N},
//	  "identity": {"mediaType":"...","digest":"sha256:...","size":N},
//	  "pulledAt": "<RFC3339>", "platforms": ["linux/arm64"],
//	  "holder": "cfs-build-..." }
//
// A record without a holder is an agent's: gc keeps the image it names. A
// record with one is a runtime reference, written by a pull that holds the
// image instead of naming it durably: it names the image on the holder's
// behalf and pins nothing, and gc deletes it once that holder no longer
// stands on the identity.
//
// The two descriptors say different things. "resolved" is provenance: the
// document the reference itself landed on, an image index for a tag or an
// index-pinned ref and an image manifest otherwise. It is unauthenticated local
// bookkeeping -- an operator reads it to see what a pull anchored on, and no
// trust decision takes it as an input; gc, though, resolves it as a wanted
// root to keep the pulled document reachable, so a record whose resolved
// chain is broken aborts a collection. "identity" is the platform image manifest
// the reference names, the one digest the object store, the image bindings, and
// the holds are all keyed by; every lookup a verb makes goes through it.
package refstore

import (
	"context"
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
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/digestutil"
	"github.com/foundriesio/ocicfs/internal/durable"
	"github.com/foundriesio/ocicfs/internal/holdstore"
)

// Store writes reference provenance under the ocicfs store root.
type Store struct {
	root string
}

// Record is the validated reference record stored for a pulled image. Resolved
// is the descriptor the reference itself landed on (provenance); Identity is
// the platform image manifest the image is keyed by.
type Record struct {
	Ref       string
	Resolved  ocispec.Descriptor
	Identity  ocispec.Descriptor
	PulledAt  time.Time
	Platforms []string
	// Holder names the hold a runtime reference names the image for; empty
	// for an agent's reference, which keeps the image on its own.
	Holder string
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

// Put writes refs/sha256/<refkey-xx>/<refkey-rest>.json atomically. resolved is
// the descriptor the reference landed on and identity the platform image
// manifest it names; the two are the same descriptor when the reference already
// names a manifest. An agent's record: it replaces whatever record the
// reference had, a runtime reference included.
func (s *Store) Put(ref string, resolved, identity ocispec.Descriptor, pulledAt time.Time, platforms []string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return s.put(ref, resolved, identity, pulledAt, platforms, "")
}

// PutHeld writes the record of a runtime reference: Put, with holder as the
// hold the record names the image for. It never replaces an agent's record —
// a reference the agent named keeps naming what the agent pulled, and the
// runtime's image stays held but unnamed — and reports whether it installed
// the record. The holder must be a name the hold store accepts. The check
// and the write are one step under the store's write lock, so an agent's
// record landing meanwhile is never overwritten.
func (s *Store) PutHeld(ref string, resolved, identity ocispec.Descriptor, pulledAt time.Time, platforms []string, holder string) (bool, error) {
	if err := holdstore.ValidateHolder(holder); err != nil {
		return false, err
	}
	unlock, err := s.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	if existing, err := s.Get(ref); err == nil && existing.Holder == "" {
		return false, nil
	}
	if err := s.put(ref, resolved, identity, pulledAt, platforms, holder); err != nil {
		return false, err
	}
	return true, nil
}

// lock takes the store's write lock — one flock on refs/.lock, created on
// demand and never removed — and returns its release. Record writes are
// serialized by it so a runtime reference's check-then-write cannot
// interleave with an agent's write of the same reference; reads take no
// lock, a record being an atomically renamed file. The wait is not
// interruptible; writes are short.
func (s *Store) lock() (func(), error) {
	path := filepath.Join(s.root, "refs", ".lock")
	if err := durable.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("refstore: create refs dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- store-owned lock path
	if err != nil {
		return nil, fmt.Errorf("refstore: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { // #nosec G115 -- a file descriptor always fits in int
		if cerr := f.Close(); cerr != nil {
			slog.Warn("refstore: failed to close lock file", "path", path, "err", cerr)
		}
		return nil, fmt.Errorf("refstore: lock: %w", err)
	}
	return func() {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil { // #nosec G115 -- a file descriptor always fits in int
			slog.Warn("refstore: failed to unlock", "path", path, "err", err)
		}
		if err := f.Close(); err != nil {
			slog.Warn("refstore: failed to close lock file", "path", path, "err", err)
		}
	}, nil
}

func (s *Store) put(ref string, resolved, identity ocispec.Descriptor, pulledAt time.Time, platforms []string, holder string) error {
	path, refKey := s.refPath(ref)
	doc := refDoc{
		Schema: refSchema,
		Ref:    ref,
		RefKey: refKeyDoc{
			Algorithm: "sha256",
			Digest:    refKey,
		},
		Resolved:  descriptorDoc{MediaType: resolved.MediaType, Digest: resolved.Digest, Size: resolved.Size},
		Identity:  descriptorDoc{MediaType: identity.MediaType, Digest: identity.Digest, Size: identity.Size},
		PulledAt:  pulledAt.Format(time.RFC3339),
		Platforms: platforms,
		Holder:    holder,
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
	return s.list(context.Background(), false)
}

// ListStrict returns all reference records sorted by exact ref string,
// failing on a record that does not decode and validate. It is the census gc
// marks wanted content from: where List's lenient skip suits verbs that only
// report, a record skipped here would silently unprotect its image from the
// sweep, so a corrupt record is an error naming the file to repair or remove.
func (s *Store) ListStrict(ctx context.Context) ([]Record, error) {
	return s.list(ctx, true)
}

func (s *Store) list(ctx context.Context, strict bool) ([]Record, error) {
	base := filepath.Join(s.root, "refs", "sha256")
	var records []Record
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("refstore: access %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
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
			if strict {
				return fmt.Errorf("refstore: invalid ref record %s: %w", path, err)
			}
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

// PruneEmptyDirs removes the empty shard directories record removal leaves
// behind. Call it only under the store-exclusive gc lock.
func (s *Store) PruneEmptyDirs() error {
	if err := durable.PruneEmptyDirs(filepath.Join(s.root, "refs", "sha256"), 1); err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	return nil
}

const refSchema = "application/vnd.ocicfs.ref.v2+json"

// The on-disk ref JSON documents.
type (
	refDoc struct {
		Schema    string        `json:"schema"`
		Ref       string        `json:"ref"`
		RefKey    refKeyDoc     `json:"refKey"`
		Resolved  descriptorDoc `json:"resolved"`
		Identity  descriptorDoc `json:"identity"`
		PulledAt  string        `json:"pulledAt"`
		Platforms []string      `json:"platforms"`
		Holder    string        `json:"holder,omitempty"`
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
		// A record this version cannot read is not repaired in place: the digests
		// it keys on may mean something else entirely, so it fails closed and the
		// remedy is to recreate the store and pull again.
		return Record{}, fmt.Errorf("unsupported ref schema %q, want %q: recreate the store and re-pull", doc.Schema, refSchema)
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
	if err := digestutil.ValidateSHA256(doc.Identity.Digest); err != nil {
		return Record{}, fmt.Errorf("identity digest: %w", err)
	}
	if doc.Identity.Size < 0 {
		return Record{}, fmt.Errorf("identity size is negative: %d", doc.Identity.Size)
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
	if doc.Holder != "" {
		if err := holdstore.ValidateHolder(doc.Holder); err != nil {
			return Record{}, fmt.Errorf("holder: %w", err)
		}
	}
	return Record{
		Ref: doc.Ref,
		Resolved: ocispec.Descriptor{
			MediaType: doc.Resolved.MediaType,
			Digest:    doc.Resolved.Digest,
			Size:      doc.Resolved.Size,
		},
		Identity: ocispec.Descriptor{
			MediaType: doc.Identity.MediaType,
			Digest:    doc.Identity.Digest,
			Size:      doc.Identity.Size,
		},
		PulledAt:  pulledAt,
		Platforms: append([]string(nil), doc.Platforms...),
		Holder:    doc.Holder,
	}, nil
}

func key(ref string) digest.Digest {
	sum := sha256.Sum256([]byte(ref))
	return digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:]))
}
