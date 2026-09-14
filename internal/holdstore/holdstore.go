// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package holdstore records GC-protection holds: named holders that pin an
// image's identity digest so a remove declines while any holder is present. A
// hold is an empty marker file under the digest's holder directory:
//
//	<root>/holds/sha256/<aa>/<rest>/<holder>
//
// keyed by the identity digest -- the platform image manifest (holds protect
// content identity, and the reference records are auxiliary). Every verb
// resolves down to that one digest before it marks, drops, or checks a holder,
// so an image has exactly one holder directory and one lock however the caller
// named it: a hold errors rather than mark a digest whose stored blob does not
// hash to it, while a release drops the holder whatever state the store is in.
// Mutating a digest's holders — and any check-then-act
// that must be atomic against them, such as a remove reading the holders and
// then deleting the image — runs under the digest's advisory lock, taken with
// Lock. The lock file lives under a dedicated area and is never removed, so a
// last-holder cleanup that removes the holder directory cannot invalidate a
// lock a concurrent caller is waiting on:
//
//	<root>/holds/.locks/sha256/<aa>/<rest>
//
// The store owns no locking of its own: every method other than Lock reads or
// writes files directly and must be called while its caller holds the digest's
// Lock.
package holdstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/durable"
)

// Store records holds under the knarr store root.
type Store struct {
	root string
}

// New opens a hold store rooted at root.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("holdstore: empty store root")
	}
	return &Store{root: root}, nil
}

// Lock acquires the exclusive advisory lock for root's holders and returns a
// release function that unlocks and closes the lock file. Hold, Release,
// Holders, and Clear all mutate or read state a caller must serialize, so they
// run under this lock; a compound check-then-act (a remove that reads the
// holders and then deletes the image) holds it across the whole sequence. The
// lock file is created on demand and never removed.
//
// The acquisition blocks until the lock is free; it is not interruptible.
// Contention is between the local agent and the snapshotter and resolves in
// well under a second, so callers recheck their context after acquiring rather
// than polling the lock, and a caller that must not proceed on a canceled
// context checks ctx.Err after Lock returns.
func (s *Store) Lock(root digest.Digest) (func(), error) {
	path, err := s.lockPath(root)
	if err != nil {
		return nil, err
	}
	if err := durable.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("holdstore: create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- digest-derived lock path
	if err != nil {
		return nil, fmt.Errorf("holdstore: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { // #nosec G115 -- a file descriptor always fits in int
		if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrNotExist) {
			slog.Warn("holdstore: failed to close lock file", "path", path, "err", cerr)
		}
		return nil, fmt.Errorf("holdstore: lock: %w", err)
	}
	return func() {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil { // #nosec G115 -- a file descriptor always fits in int
			slog.Warn("holdstore: failed to unlock", "path", path, "err", err)
		}
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("holdstore: failed to close lock file", "path", path, "err", err)
		}
	}, nil
}

// Hold records holder as a holder of root, creating the holder directory on
// first use. It is idempotent: recording an already-present holder succeeds
// without change. Call under Lock.
func (s *Store) Hold(root digest.Digest, holder string) error {
	if err := ValidateHolder(holder); err != nil {
		return err
	}
	dir, err := s.holderDir(root)
	if err != nil {
		return err
	}
	if err := durable.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("holdstore: create holder dir: %w", err)
	}
	// Mark, not WriteFile: the holder directory is enumerated as state, so it
	// must never contain a temp file a crash could strand and Holders would count
	// as a holder.
	if err := durable.Mark(filepath.Join(dir, holder), 0o644); err != nil {
		return fmt.Errorf("holdstore: write holder: %w", err)
	}
	return nil
}

// Release drops holder from root's holders, removing the holder directory once
// its last holder is gone. It is idempotent: releasing a holder that is not
// present succeeds. Call under Lock.
func (s *Store) Release(root digest.Digest, holder string) error {
	if err := ValidateHolder(holder); err != nil {
		return err
	}
	dir, err := s.holderDir(root)
	if err != nil {
		return err
	}
	// A durable removal that finds neither the marker nor its directory is a
	// no-op success: the digest was never held (durable.Remove surfaces the
	// absent parent directory as ErrNotExist from its parent fsync).
	if err := durable.Remove(filepath.Join(dir, holder)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("holdstore: remove holder: %w", err)
	}
	// Best-effort cleanup of the now-empty holder directory. A remaining holder
	// leaves it non-empty (an expected condition), and a concurrent last release
	// may already have removed it; anything else is an unexpected failure worth a
	// warning but not worth failing the release.
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !isNotEmpty(err) {
		slog.Warn("holdstore: failed to remove empty holder dir", "path", dir, "err", err)
	}
	return nil
}

// Holders returns the sorted holder names recorded for root, or an empty slice
// when none are. Call under Lock; a lock-free call is permitted only for an
// advisory read that tolerates racing with a release, in either direction.
func (s *Store) Holders(root digest.Digest) ([]string, error) {
	dir, err := s.holderDir(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("holdstore: read holders: %w", err)
	}
	holders := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		holders = append(holders, e.Name())
	}
	sort.Strings(holders)
	return holders, nil
}

// Clear removes every holder of root, each removal made crash-durable like a
// Release. It is idempotent: clearing a digest with no holders succeeds. Call
// under Lock.
func (s *Store) Clear(root digest.Digest) error {
	dir, err := s.holderDir(root)
	if err != nil {
		return err
	}
	holders, err := s.Holders(root)
	if err != nil {
		return err
	}
	for _, h := range holders {
		if err := durable.Remove(filepath.Join(dir, h)); err != nil {
			return fmt.Errorf("holdstore: clear holder %q: %w", h, err)
		}
	}
	// Best-effort cleanup of the now-empty holder directory, tolerating an absent
	// or (under a racing add that the caller's lock should preclude) non-empty
	// directory.
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !isNotEmpty(err) {
		slog.Warn("holdstore: failed to remove empty holder dir", "path", dir, "err", err)
	}
	return nil
}

// List returns every digest that has at least one holder, sorted. It is
// strict where the per-digest readers are lenient: gc marks wanted content
// from this census, so an entry under holds/sha256 that does not reconstruct
// into a valid digest is an error rather than a skip — skipped, it would
// silently unprotect whatever its holders pinned. An empty holder directory
// (a crash between a last release's unlink and its rmdir) has no holders and
// is not listed. No per-digest locks are taken: gc reads the census under the
// store's exclusive lock, which excludes every mutating verb; a lock-free
// reader racing a last release may find a holder directory gone between the
// walk and the read, which is a digest with no holders, not an error.
func (s *Store) List(ctx context.Context) ([]digest.Digest, error) {
	base := filepath.Join(s.root, "holds", "sha256")
	shards, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("holdstore: read holds: %w", err)
	}
	var out []digest.Digest
	for _, shard := range shards {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !shard.IsDir() || len(shard.Name()) != 2 {
			return nil, fmt.Errorf("holdstore: foreign entry %q under holds", shard.Name())
		}
		rests, err := os.ReadDir(filepath.Join(base, shard.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("holdstore: read holds shard %s: %w", shard.Name(), err)
		}
		for _, rest := range rests {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			d := digest.NewDigestFromEncoded(digest.SHA256, shard.Name()+rest.Name())
			if !rest.IsDir() || digestutil.ValidateSHA256(d) != nil {
				return nil, fmt.Errorf("holdstore: foreign entry %q under holds shard %s", rest.Name(), shard.Name())
			}
			holders, err := os.ReadDir(filepath.Join(base, shard.Name(), rest.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("holdstore: read holders of %s: %w", d, err)
			}
			if len(holders) > 0 {
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// PruneEmptyDirs removes the empty directories the hold tree accumulates: a
// crash-stranded holder directory and the shard level above it. The .locks
// tree is left alone — removing a lock file whose flock a process still rides
// would split the lock's identity. Call it only under the store-exclusive gc
// lock.
func (s *Store) PruneEmptyDirs() error {
	if err := durable.PruneEmptyDirs(filepath.Join(s.root, "holds", "sha256"), 2); err != nil {
		return fmt.Errorf("holdstore: %w", err)
	}
	return nil
}

func (s *Store) holderDir(root digest.Digest) (string, error) {
	if err := digestutil.ValidateSHA256(root); err != nil {
		return "", fmt.Errorf("holdstore: root digest: %w", err)
	}
	hex := root.Encoded()
	return filepath.Join(s.root, "holds", "sha256", hex[:2], hex[2:]), nil
}

func (s *Store) lockPath(root digest.Digest) (string, error) {
	if err := digestutil.ValidateSHA256(root); err != nil {
		return "", fmt.Errorf("holdstore: root digest: %w", err)
	}
	hex := root.Encoded()
	return filepath.Join(s.root, "holds", ".locks", "sha256", hex[:2], hex[2:]), nil
}

// ValidateHolder rejects a holder name that is empty, "." or "..", or carries
// any character outside [a-z0-9._-] (which excludes the path separator), so a
// holder name can only ever name a single file inside a digest's holder
// directory. Hold and Release validate their holder themselves; a caller that
// takes a holder name long before it holds can reject a bad one up front.
func ValidateHolder(holder string) error {
	if holder == "" {
		return errors.New("holdstore: empty holder name")
	}
	if holder == "." || holder == ".." {
		return fmt.Errorf("holdstore: invalid holder name %q", holder)
	}
	for _, r := range holder {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return fmt.Errorf("holdstore: invalid holder name %q: allowed characters are a-z, 0-9, '.', '_', '-'", holder)
		}
	}
	return nil
}

// isNotEmpty reports whether err is a directory-not-empty rmdir error, which
// Linux reports as ENOTEMPTY and some systems as EEXIST.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
