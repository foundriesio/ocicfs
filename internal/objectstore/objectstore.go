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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/durable"
	"github.com/foundriesio/knarr/internal/verity"
)

// verityDirName is the fs-verity view directory under objects/. It is named
// after its key algorithm's upstream identifier, mirroring the objects/sha256
// convention — self-description, not a multi-algorithm switch.
const verityDirName = "fsverity-sha256-12"

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
	if err := os.MkdirAll(s.viewPath(""), 0o755); err != nil { // #nosec G301 -- knarr-owned store dir
		return nil, err
	}
	if err := os.MkdirAll(s.TempDir(), 0o755); err != nil { // #nosec G301 -- knarr-owned store dir
		return nil, err
	}
	return s, nil
}

// ObjectPath returns the CAS path for stored object d.
func (s *Store) ObjectPath(d digest.Digest) (string, error) {
	if err := digestutil.ValidateSHA256(d); err != nil {
		return "", fmt.Errorf("objectstore: object digest: %w", err)
	}
	return s.objectPath(d.Hex()), nil
}

// ObjectsDir returns the composefs backing-object directory.
func (s *Store) ObjectsDir() string {
	return s.objectPath("")
}

// ViewDir returns the fs-verity view directory: a tree of hardlinks to the CAS
// objects, sharded and keyed by each object's measured fs-verity digest.
func (s *Store) ViewDir() string {
	return s.viewPath("")
}

// Open returns a reader over the stored object d. The caller must close it. A
// missing object surfaces as an error satisfying errors.Is(err, os.ErrNotExist).
func (s *Store) Open(d digest.Digest) (io.ReadCloser, error) {
	path, err := s.ObjectPath(d)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0) // #nosec G304 -- digest-derived object path
	if err != nil {
		return nil, fmt.Errorf("objectstore: open object %s: %w", d, err)
	}
	fi, err := f.Stat()
	if err != nil {
		closeErr := f.Close()
		err = fmt.Errorf("objectstore: stat object %s: %w", d, err)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("objectstore: close object %s: %w", d, closeErr))
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		closeErr := f.Close()
		err := fmt.Errorf("objectstore: object %s is not a regular file", d)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("objectstore: close object %s: %w", d, closeErr))
		}
		return nil, err
	}
	return f, nil
}

// Has reports whether the object d is present as a regular file in the store.
// A missing path is reported as (false, nil); a non-regular node at the path
// (a directory, symlink, or other special file) is not a stored object and
// reports (false, nil) too. Any other stat failure is returned. Lstat is used
// so a symlink is judged on its own type, never followed.
func (s *Store) Has(d digest.Digest) (bool, error) {
	path, err := s.ObjectPath(d)
	if err != nil {
		return false, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("objectstore: stat object %s: %w", d, err)
	}
	return fi.Mode().IsRegular(), nil
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
	tempPath, d, err := s.stage(ctx, tempPrefix, expect, r)
	if err != nil {
		return "", "", err
	}
	fsv, err := s.seal(ctx, tempPath, d)
	if err != nil {
		return "", "", err
	}
	return d, fsv, nil
}

// stage streams r into a unique store temp while hashing and returns the temp
// path and the computed content digest. When expect is non-empty the bytes must
// hash to it. The temp is closed and owned by the caller on success; on any
// error stage removes it and returns nothing.
func (s *Store) stage(ctx context.Context, tempPrefix string, expect digest.Digest, r io.Reader) (string, digest.Digest, error) {
	if err := ctx.Err(); err != nil {
		return "", "", fmt.Errorf("objectstore: %w", err)
	}
	f, err := os.CreateTemp(s.TempDir(), tempPrefix) // #nosec G304 -- store-owned temp dir
	if err != nil {
		return "", "", fmt.Errorf("objectstore: create object temp: %w", err)
	}
	tempPath := f.Name()
	fail := func(err error) (string, digest.Digest, error) {
		_ = f.Close()
		removeTemp(tempPath)
		return "", "", err
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: r}); err != nil {
		return fail(fmt.Errorf("objectstore: write %s: %w", tempPath, err))
	}
	d := digest.NewDigest(digest.SHA256, h)

	// Reject mismatched content before it can be sealed or committed, so a bad
	// input leaves no object behind.
	if expect != "" && d != expect {
		return fail(fmt.Errorf("objectstore: blob digest mismatch: expected %s, got %s", expect, d))
	}

	// fs-verity cannot be enabled while a writable descriptor is open, so the
	// writable handle is closed here and seal reopens the temp read-only. The
	// window in between is not a tampering vector: the temp was created 0600 in
	// the store's private temp dir, owned by the single privileged knarr process,
	// so no other principal can open it for writing (one that already holds that
	// privilege is outside the threat model).
	if err := f.Close(); err != nil {
		removeTemp(tempPath)
		return "", "", fmt.Errorf("objectstore: close temp: %w", err)
	}
	return tempPath, d, nil
}

// seal enables fs-verity on the staged temp whose content digest is d, measures
// it, and commits it at the content-addressed path with no-replace semantics,
// returning the measured fs-verity digest. On a collision it confirms the
// existing object's measured fs-verity digest matches the staged one rather
// than overwriting an immutable object, and returns that object's digest. The
// temp is consumed on every path: renamed into place on success, removed
// otherwise. Once enabled, fs-verity makes the bytes immutable and
// kernel-verified on every read for the life of the object. No fsync is taken
// per object: a pull makes all of its writes durable with a single store-wide
// Sync before it writes the binding that references them.
func (s *Store) seal(ctx context.Context, tempPath string, d digest.Digest) (string, error) {
	defer removeTemp(tempPath)

	// Stop before sealing if the pull was canceled while the temp was staged,
	// so an abandoned object is not made permanent.
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("objectstore: %w", err)
	}
	rf, err := os.Open(tempPath) // #nosec G304 -- store-owned temp path
	if err != nil {
		return "", fmt.Errorf("objectstore: reopen temp: %w", err)
	}
	if err := verity.Enable(rf); err != nil {
		_ = rf.Close()
		return "", fmt.Errorf("objectstore: enable verity on temp: %w", err)
	}
	// The enable ioctl cannot be interrupted, so check again before the object
	// is committed: a pull canceled during the ioctl must not publish it.
	if err := ctx.Err(); err != nil {
		_ = rf.Close()
		return "", fmt.Errorf("objectstore: %w", err)
	}
	fsv, err := verity.Measure(rf)
	if err != nil {
		_ = rf.Close()
		return "", fmt.Errorf("objectstore: measure verity: %w", err)
	}
	if err := rf.Close(); err != nil {
		return "", fmt.Errorf("objectstore: close verity temp: %w", err)
	}

	finalPath := s.objectPath(d.Hex())
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil { // #nosec G301 -- knarr-owned object-store shard dir
		return "", fmt.Errorf("objectstore: create shard dir: %w", err)
	}
	// Commit with no-replace semantics. On collision compare the staged object
	// against the existing one rather than overwriting an immutable object.
	if err := commitNoReplace(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			existingFsv, vErr := s.verifyCollision(finalPath, fsv)
			if vErr != nil {
				return "", vErr
			}
			// The object was already present; repair the view link in case a
			// prior write committed the object but not its view name.
			if err := s.linkView(finalPath, existingFsv); err != nil {
				return "", err
			}
			return existingFsv, nil
		}
		return "", fmt.Errorf("objectstore: commit to %s: %w", finalPath, err)
	}
	if err := s.linkView(finalPath, fsv); err != nil {
		return "", err
	}
	return fsv, nil
}

// linkView hardlinks the committed object at objectPath into the fs-verity view
// under objects/fsverity-sha256-12/, sharded like the sha256 namespace and keyed
// by the object's measured fs-verity digest fsv ("sha256:<hex>"). Both names
// share one inode, so fs-verity carries over. The link failing fails the write:
// no write succeeds without its view name (a crash between the CAS commit and
// the link leaves an object the next collision repairs). A view name that already resolves
// to the same inode is accepted — a concurrent writer of identical content, or a
// collision repairing its own link; a different inode at the view name is store
// corruption.
func (s *Store) linkView(objectPath, fsv string) error {
	viewPath := s.viewPath(strings.TrimPrefix(fsv, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(viewPath), 0o755); err != nil { // #nosec G301 -- knarr-owned view shard dir
		return fmt.Errorf("objectstore: create view shard dir: %w", err)
	}
	err := os.Link(objectPath, viewPath)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		objFI, statErr := os.Lstat(objectPath)
		if statErr != nil {
			return fmt.Errorf("objectstore: stat object %s: %w", objectPath, statErr)
		}
		viewFI, statErr := os.Lstat(viewPath)
		if statErr != nil {
			return fmt.Errorf("objectstore: stat view %s: %w", viewPath, statErr)
		}
		if !os.SameFile(objFI, viewFI) {
			return fmt.Errorf("objectstore: view name %s does not resolve to object %s", viewPath, objectPath)
		}
		return nil
	default:
		return fmt.Errorf("objectstore: link view %s: %w", viewPath, err)
	}
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
// at the content-addressed path. It returns the computed content digest and an
// already-resolved Sealed carrying the measured fs-verity digest. The object's
// digest is not known in advance: it is whatever the bytes hash to. On a
// collision it confirms the existing object's measured fs-verity digest matches
// the staged one rather than overwriting it, and reports that object's
// measured fs-verity digest.
func (s *Store) WriteObject(ctx context.Context, r io.Reader) (digest.Digest, Sealed, error) {
	d, fsv, err := s.writeSealed(ctx, ".object-*", "", r)
	if err != nil {
		return "", nil, err
	}
	return d, Resolved(fsv), nil
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
	path, err := s.ObjectPath(d)
	if err != nil {
		return "", err
	}
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

// ErrInUse reports that an object cannot be removed because a mounter holds
// the shared in-use lock on it.
var ErrInUse = errors.New("objectstore: object in use")

// Walk calls fn for every stored object with its digest and size. An entry
// that does not have the store's two-level sha256 shard shape, or is not a
// regular file, is skipped with a warning rather than failing the walk: a
// foreign entry is not a stored object, and skipping it only retains it.
func (s *Store) Walk(ctx context.Context, fn func(d digest.Digest, size int64) error) error {
	base := s.objectPath("")
	return filepath.WalkDir(base, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("objectstore: walk objects: %w", walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() {
			return nil
		}
		d, ok := shardedDigest(base, path)
		if !ok || !de.Type().IsRegular() {
			slog.Warn("objectstore: skipping foreign entry in object store", "path", path)
			return nil
		}
		fi, err := de.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("objectstore: stat object %s: %w", d, err)
		}
		return fn(d, fi.Size())
	})
}

// Remove unlinks the stored object d, both its CAS name and its fs-verity
// view name, unless a mounter holds the shared in-use lock on it, in which
// case it returns an error matching ErrInUse. The unlinks happen while the
// exclusive lock is held, pairing with the mount side, which takes the shared
// lock and then refuses an inode whose name count dropped to zero. The view
// name goes first: a crash between the unlinks then leaves the authoritative
// CAS object with a missing view link, which the next deduplicating write
// repairs, where the reverse order would leave an orphan view inode that
// fails that write's same-inode check. An absent object removes nothing and
// succeeds.
func (s *Store) Remove(d digest.Digest) error {
	path, err := s.ObjectPath(d)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0) // #nosec G304 -- digest-derived object path
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("objectstore: open object %s: %w", d, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("objectstore: failed to close object", "digest", d, "err", err)
		}
	}()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor always fits in int
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("objectstore: object %s: %w", d, ErrInUse)
		}
		return fmt.Errorf("objectstore: lock object %s: %w", d, err)
	}
	fsv, err := verity.Measure(f)
	if err != nil {
		return fmt.Errorf("objectstore: measure object %s: %w", d, err)
	}
	viewPath := s.viewPath(strings.TrimPrefix(fsv, "sha256:"))
	if err := os.Remove(viewPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("objectstore: remove view name %s: %w", viewPath, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("objectstore: remove object %s: %w", d, err)
	}
	return nil
}

// InUse reports whether a mounter currently holds the shared in-use lock on
// object d: it probes with a non-blocking exclusive flock and releases
// immediately on success. An absent object is not in use.
func (s *Store) InUse(d digest.Digest) (bool, error) {
	path, err := s.ObjectPath(d)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0) // #nosec G304 -- digest-derived object path
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("objectstore: open object %s: %w", d, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("objectstore: failed to close object", "digest", d, "err", err)
		}
	}()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- a file descriptor always fits in int
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("objectstore: probe object %s: %w", d, err)
	}
	// The close below drops the probe lock with the descriptor.
	return false, nil
}

// SweepViewOrphans removes fs-verity view entries whose CAS partner is gone —
// the view name is always a second hardlink to a CAS object, so a view file
// whose inode has a single name lost its partner to external damage. An
// orphan whose view-relative name the keep predicate claims is preserved
// with a warning instead of deleted: the view name is the copy mounts
// actually serve, so a claimed orphan is the last copy of wanted content,
// not garbage. A nil predicate keeps nothing. It returns the number of
// entries removed.
func (s *Store) SweepViewOrphans(ctx context.Context, keep func(name string) bool) (int, error) {
	base := s.viewPath("")
	removed := 0
	err := filepath.WalkDir(base, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("objectstore: walk view: %w", walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() || !de.Type().IsRegular() {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return fmt.Errorf("objectstore: stat view entry %s: %w", path, err)
		}
		if st.Nlink != 1 {
			return nil
		}
		if keep != nil {
			if rel, err := filepath.Rel(base, path); err == nil && keep(filepath.ToSlash(rel)) {
				slog.Warn("objectstore: keeping wanted view entry whose CAS name is missing", "path", path)
				return nil
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G122 -- knarr-private store tree, path from the walk itself
			return fmt.Errorf("objectstore: remove view orphan %s: %w", path, err)
		}
		removed++
		return nil
	})
	return removed, err
}

// PruneEmptyDirs removes the empty shard directories object removal leaves
// behind, in both the CAS tree and the fs-verity view. Call it only under the
// store-exclusive gc lock, which excludes every writer that could be
// re-creating a shard.
func (s *Store) PruneEmptyDirs() error {
	for _, dir := range []string{s.objectPath(""), s.viewPath("")} {
		if err := durable.PruneEmptyDirs(dir, 1); err != nil {
			return fmt.Errorf("objectstore: %w", err)
		}
	}
	return nil
}

// removeTemp removes a staging temp, logging any failure other than the temp
// already being gone (renamed into place, or removed on an earlier error path).
func removeTemp(tempPath string) {
	if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("objectstore: failed to remove temp file", "path", tempPath, "err", err)
	}
}

// shardedDigest reconstructs the sha256 digest a store path names, reporting
// false for a path that does not have the <aa>/<rest> shard shape of a valid
// digest.
func shardedDigest(base, path string) (digest.Digest, bool) {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return "", false
	}
	shard, rest, found := strings.Cut(rel, string(filepath.Separator))
	if !found || len(shard) != 2 || strings.ContainsRune(rest, filepath.Separator) {
		return "", false
	}
	d := digest.NewDigestFromEncoded(digest.SHA256, shard+rest)
	if err := digestutil.ValidateSHA256(d); err != nil {
		return "", false
	}
	return d, true
}

// objectPath returns the on-disk path for a given digest hex string.
func (s *Store) objectPath(hexDigest string) string {
	if len(hexDigest) < 2 {
		return filepath.Join(s.root, "objects", "sha256")
	}
	return filepath.Join(s.root, "objects", "sha256", hexDigest[0:2], hexDigest[2:])
}

// viewPath returns the on-disk view path for a given fs-verity digest hex string,
// sharded like objectPath.
func (s *Store) viewPath(verityHex string) string {
	if len(verityHex) < 2 {
		return filepath.Join(s.root, "objects", verityDirName)
	}
	return filepath.Join(s.root, "objects", verityDirName, verityHex[0:2], verityHex[2:])
}
