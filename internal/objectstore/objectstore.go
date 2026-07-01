// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package objectstore is the immutable, sha256-keyed content-addressed object
// store. It holds the decompressed per-file objects, the OCI metadata blobs, and
// the generated composefs image, enables fs-verity on objects, and commits with
// no-replace semantics. It also owns the build temp dir. Concurrent writers are
// safe by construction: every write goes to a unique temp, every commit is
// no-replace, and a collision re-verifies the existing object, so no store-wide
// lock is needed.
//
// The store is private to the knarr process, which runs privileged and performs
// the mount itself: object files are owner-only (0600) and protected by
// fs-verity, so the digest store does not need to be world-readable. Objects are
// not fsynced individually; a pull makes all of its writes durable with a single
// store-wide Sync before it writes the binding that references them, so a crash
// leaves either a complete, durable pull or one with no binding. An interrupted
// pull is recovered by re-running it, since every write is idempotent.
package objectstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/verity"
)

// Store is the content-addressed object store rooted at the knarr data directory.
// Its objects live under <root>/objects and transient build state under <root>/tmp.
type Store struct {
	root string
}

// New opens the content-addressed store under root, creating its directories
// on first use.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("objectstore: empty store root")
	}
	s := &Store{root: root}
	// Create store directories on first use. The directories are traversable
	// (0755) but the object files within are owner-only (0600); the store is
	// read by the privileged knarr process that performs the mount.
	if err := os.MkdirAll(s.objectPath(""), 0o755); err != nil { // #nosec G301 -- knarr-owned store dir
		return nil, err
	}
	if err := os.MkdirAll(s.TempDir(), 0o755); err != nil { // #nosec G301 -- knarr-owned store dir
		return nil, err
	}
	return s, nil
}

// CheckVerity verifies the store filesystem can enable fs-verity, which knarr
// requires for every content object. It enables verity on a throwaway probe file
// so a pull fails fast with a clear error when the store is on a filesystem
// without fs-verity support, rather than partway through writing objects.
func (s *Store) CheckVerity() error {
	f, err := os.CreateTemp(s.TempDir(), ".verity-check-*") // #nosec G304 -- store-owned temp dir
	if err != nil {
		return fmt.Errorf("objectstore: verity preflight: %w", err)
	}
	name := f.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("objectstore: failed to remove verity-preflight temp file", "path", name, "err", err)
		}
	}()
	// fs-verity may reject a zero-length file, so give the probe some content;
	// the bytes are irrelevant. Close the writable handle before enabling.
	if _, err := f.WriteString("knarr-verity-preflight"); err != nil {
		_ = f.Close()
		return fmt.Errorf("objectstore: verity preflight: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("objectstore: verity preflight: %w", err)
	}

	rf, err := os.Open(name) // #nosec G304 -- store-owned temp path
	if err != nil {
		return fmt.Errorf("objectstore: verity preflight: %w", err)
	}
	defer func() { _ = rf.Close() }()
	if err := verity.Enable(rf); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) {
			return fmt.Errorf("the store at %s is not on an fs-verity-capable filesystem, which knarr requires; point the store root at a filesystem with fs-verity support: %w", s.root, err)
		}
		if errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("the kernel on this host cannot build a %d-byte fs-verity Merkle block (page size %d), which knarr requires for stable per-file digests: %w", verity.BlockSize, os.Getpagesize(), err)
		}
		return fmt.Errorf("objectstore: verity preflight: %w", err)
	}
	return nil
}

// writeSealed streams r into a unique store temp while hashing, seals it with
// fs-verity, and commits it at the content-addressed path with no-replace
// semantics. It returns the computed content digest and the measured fs-verity
// digest. tempPrefix names the staging file. When expect is non-empty the staged
// bytes must hash to it; on a mismatch the temp is discarded and nothing is
// committed. On a collision it confirms the existing object's measured fs-verity
// digest matches the staged one rather than overwriting an immutable object, and
// returns that object's measured fs-verity digest.
func (s *Store) writeSealed(ctx context.Context, tempPrefix string, expect digest.Digest, r io.Reader) (digest.Digest, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", fmt.Errorf("objectstore: %w", err)
	}
	f, err := os.CreateTemp(s.TempDir(), tempPrefix) // #nosec G304 -- store-owned temp dir
	if err != nil {
		return "", "", fmt.Errorf("objectstore: create object temp: %w", err)
	}
	tempPath := f.Name()
	defer func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("objectstore: failed to remove temp file", "path", tempPath, "err", err)
		}
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: r}); err != nil {
		_ = f.Close()
		return "", "", fmt.Errorf("objectstore: write %s: %w", tempPath, err)
	}
	d := digest.NewDigest(digest.SHA256, h)

	// Reject mismatched content before sealing or committing, so a bad input
	// leaves no object behind (the temp is removed by the deferred cleanup).
	if expect != "" && d != expect {
		_ = f.Close()
		return "", "", fmt.Errorf("objectstore: blob digest mismatch: expected %s, got %s", expect, d)
	}

	// Stop before sealing and committing if the pull was canceled while the copy
	// completed, so an abandoned object is not made permanent (its temp is removed
	// by the deferred cleanup).
	if err := ctx.Err(); err != nil {
		_ = f.Close()
		return "", "", fmt.Errorf("objectstore: %w", err)
	}

	// fs-verity cannot be enabled while a writable descriptor is open, so close
	// the writable handle, then reopen read-only to enable and measure verity.
	// The window between close and enable is not a tampering vector: the temp was
	// created 0600 in the store's private temp dir, owned by the single privileged
	// knarr process, so no other principal can open it for writing (one that already
	// holds that privilege is outside the threat model). Once enabled, fs-verity
	// makes the bytes immutable and kernel-verified on every read for the life of the
	// object.
	// No fsync is taken per object: a pull makes all of its writes durable with a
	// single store-wide Sync before it writes the binding that references them.
	if err := f.Close(); err != nil {
		return "", "", fmt.Errorf("objectstore: close temp: %w", err)
	}

	rf, err := os.Open(tempPath) // #nosec G304 -- store-owned temp path
	if err != nil {
		return "", "", fmt.Errorf("objectstore: reopen temp: %w", err)
	}
	if err := verity.Enable(rf); err != nil {
		_ = rf.Close()
		return "", "", fmt.Errorf("objectstore: enable verity on temp: %w", err)
	}
	fsv, err := verity.Measure(rf)
	if err != nil {
		_ = rf.Close()
		return "", "", fmt.Errorf("objectstore: measure verity: %w", err)
	}
	if err := rf.Close(); err != nil {
		return "", "", fmt.Errorf("objectstore: close verity temp: %w", err)
	}

	finalPath := s.objectPath(d.Hex())
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil { // #nosec G301 -- knarr-owned object-store shard dir
		return "", "", fmt.Errorf("objectstore: create shard dir: %w", err)
	}
	// Commit with no-replace semantics. On collision compare the staged object
	// against the existing one rather than overwriting an immutable object.
	if err := commitNoReplace(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			existingFsv, vErr := s.verifyCollision(finalPath, fsv)
			return d, existingFsv, vErr
		}
		return "", "", fmt.Errorf("objectstore: commit to %s: %w", finalPath, err)
	}
	return d, fsv, nil
}

// ctxReader aborts an in-progress copy when ctx is canceled, so a large object
// stops streaming promptly rather than after the whole reader is drained.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// WriteObject streams r into the store, seals it with fs-verity, and commits it
// at the content-addressed path. It returns the computed content digest and the
// measured fs-verity digest. The object's digest is not known in advance: it is
// whatever the bytes hash to. On a collision it confirms the existing object's
// measured fs-verity digest matches the staged one rather than overwriting it,
// and returns that object's measured fs-verity digest.
func (s *Store) WriteObject(ctx context.Context, r io.Reader) (digest.Digest, string, error) {
	return s.writeSealed(ctx, ".object-*", "", r)
}

// verifyCollision confirms the object already at path is the one we just staged
// by comparing its measured fs-verity digest against the staged object's. The
// fs-verity measurement is a Merkle root over the file's content, so a match
// proves byte-for-byte content identity and that the existing object was sealed
// with the same fs-verity parameters; an O(1) metadata read replaces a full
// re-hash of the existing bytes. It returns the existing object's measured
// fs-verity digest.
func (s *Store) verifyCollision(path string, stagedFsv string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- digest-derived object-store path
	if err != nil {
		return "", fmt.Errorf("objectstore: open existing %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fsv, err := verity.Measure(f)
	if err != nil {
		return "", fmt.Errorf("objectstore: cannot measure fs-verity for %s: %w", path, err)
	}
	if fsv != stagedFsv {
		return "", fmt.Errorf("objectstore: collision at %s but fs-verity digest mismatch: existing %s, staged %s", path, fsv, stagedFsv)
	}
	return fsv, nil
}

// WriteBlob commits a metadata blob whose bytes must hash to expected, sealing it
// with fs-verity like any other content object. The digest is known in advance,
// so it validates the computed digest against expected and rejects a non-sha256
// algorithm. On a collision it re-verifies the existing bytes.
func (s *Store) WriteBlob(ctx context.Context, expected digest.Digest, r io.Reader) error {
	if expected.Algorithm() != digest.SHA256 {
		return fmt.Errorf("objectstore: WriteBlob unsupported algorithm %s", expected.Algorithm())
	}
	if _, _, err := s.writeSealed(ctx, ".blob-*", expected, r); err != nil {
		return err
	}
	return nil
}

// commitNoReplace atomically moves tempPath to finalPath without overwriting an
// existing file, returning os.ErrExist if finalPath already exists. It prefers
// renameat2(RENAME_NOREPLACE) and falls back to a hard link on filesystems or
// kernels that do not support that flag; both are atomic and refuse to clobber
// an existing immutable object. The caller's temp is removed by its own defer.
func commitNoReplace(tempPath, finalPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, tempPath, unix.AT_FDCWD, finalPath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		return os.ErrExist
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTTY):
		// RENAME_NOREPLACE unsupported here: fall back to a hard link.
		if linkErr := os.Link(tempPath, finalPath); linkErr != nil {
			if errors.Is(linkErr, os.ErrExist) {
				return os.ErrExist
			}
			return fmt.Errorf("objectstore: link commit: %w", linkErr)
		}
		return nil
	default:
		return err
	}
}

// VerityDigest returns the measured fs-verity digest of the stored object d.
func (s *Store) VerityDigest(d digest.Digest) (string, error) {
	if d.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("objectstore: unsupported digest algorithm %s", d.Algorithm())
	}
	path := s.objectPath(d.Hex())
	f, err := os.Open(path) // #nosec G304 -- digest-derived object-store path
	if err != nil {
		return "", fmt.Errorf("objectstore: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	return verity.Measure(f)
}

// Sync flushes the store's filesystem with a single syncfs so every object
// committed during a pull becomes durable in one operation. A pull calls it once
// before writing the binding that references its objects, rather than fsyncing
// each object, so a crash leaves either a complete pull or one with no binding.
func (s *Store) Sync() error {
	d, err := os.Open(s.root) // #nosec G304 -- knarr-owned store root
	if err != nil {
		return fmt.Errorf("objectstore: open store root: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := unix.Syncfs(int(d.Fd())); err != nil { // #nosec G115 -- a file descriptor always fits in int
		return fmt.Errorf("objectstore: syncfs store: %w", err)
	}
	return nil
}

// TempDir returns the store-owned temp directory used for staging and builds.
func (s *Store) TempDir() string {
	return filepath.Join(s.root, "tmp")
}

// objectPath returns the on-disk path for a given digest hex string.
func (s *Store) objectPath(hexDigest string) string {
	if len(hexDigest) < 2 {
		return filepath.Join(s.root, "objects", "sha256")
	}
	return filepath.Join(s.root, "objects", "sha256", hexDigest[0:2], hexDigest[2:])
}
