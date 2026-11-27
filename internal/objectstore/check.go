// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

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
	"syscall"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/verity"
)

// Entry is one stored object as Inventory saw it: the names its inode has.
// An object normally has both. Either may be empty: a crashed write or an
// interrupted removal leaves an object with its content-addressed name only,
// and external damage can leave either one alone.
type Entry struct {
	// Digest is the content-addressed name, empty when the object has none.
	Digest digest.Digest
	// ViewName is the view-relative name ("ab/cdef..."), empty when the
	// object has none.
	ViewName string
}

// ObjectStatus is what CheckObject found an object's bytes to be.
type ObjectStatus int

const (
	// ObjectOK: the bytes match every name the object has. An object with
	// its content-addressed name only is OK too; ObjectCheck.Fsverity then
	// carries the view name it should have.
	ObjectOK ObjectStatus = iota
	// ObjectMisnamed: the bytes match one name, and the other is absent or
	// names different content. The content is intact.
	ObjectMisnamed
	// ObjectDamaged: the bytes match no name the object has, or reading
	// them failed with an I/O or filesystem-corruption error.
	ObjectDamaged
)

// ObjectCheck is the result of checking one object.
type ObjectCheck struct {
	Status ObjectStatus
	// Digest is the sha256 of the bytes, empty when they could not be read.
	Digest digest.Digest
	// Fsverity is the fs-verity digest of the bytes ("sha256:<hex>"), empty
	// when they could not be read.
	Fsverity string
	// Detail says why a damaged or misnamed object is so.
	Detail string
	// Unopenable is set on a damaged object that could not even be opened.
	Unopenable bool
	// file is the inode that was read. RepairNames and RemoveEntry act
	// only on names that still name it. It is nil for an unopenable object.
	file fs.FileInfo
}

// checkOpen and checkReader open an object and wrap the reader CheckObject
// reads its bytes through. Tests replace them to inject failures.
var (
	checkOpen   = os.OpenFile
	checkReader = func(f *os.File) io.Reader { return f }
)

// Inventory lists every object in the store with the names it has, pairing
// the content-addressed and view names by inode. It reads no object content.
// The object tree is walked before the view, so an object committed between
// the two walks can appear with its view name only; CheckObject looks such
// an entry's content-addressed name up again. An inode with a second name
// in either tree yields one entry per extra name.
func (s *Store) Inventory(ctx context.Context) ([]Entry, error) {
	type inode struct{ dev, ino uint64 }
	var entries []Entry
	byInode := map[inode][]int{}
	err := s.walk(ctx, func(d digest.Digest, fi fs.FileInfo) error {
		st := fi.Sys().(*syscall.Stat_t)
		k := inode{dev: st.Dev, ino: st.Ino}
		byInode[k] = append(byInode[k], len(entries))
		entries = append(entries, Entry{Digest: d})
		return nil
	})
	if err != nil {
		return nil, err
	}
	base := s.viewPath("")
	err = filepath.WalkDir(base, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("objectstore: walk view: %w", walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return fmt.Errorf("objectstore: view entry %s: %w", path, err)
		}
		name := filepath.ToSlash(rel)
		if _, err := viewNameDigest("", name); err != nil || !de.Type().IsRegular() {
			slog.Warn("objectstore: skipping foreign entry in the view", "path", path)
			return nil
		}
		fi, err := de.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("objectstore: stat view entry %s: %w", path, err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		partners := byInode[inode{dev: st.Dev, ino: st.Ino}]
		if len(partners) == 0 {
			entries = append(entries, Entry{ViewName: name})
			return nil
		}
		for _, i := range partners {
			if entries[i].ViewName == "" {
				entries[i].ViewName = name
			} else {
				entries = append(entries, Entry{Digest: entries[i].Digest, ViewName: name})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// CheckObject reads the object e once and compares its bytes with each name
// it has: their sha256 with the content-addressed name, and their fs-verity
// digest with the view name. With fs-verity that digest is the kernel's
// measurement and the read makes the kernel check every block; without, it
// is computed from the read.
//
// Only evidence about the bytes makes an object damaged: a digest that
// matches no name, or an open or read that fails with an I/O or
// filesystem-corruption error. Any other failure is returned as an error and
// says nothing about the object.
//
// An object without fs-verity in a store that runs with it is hashed all the
// same. Bytes that match no name it has make it damaged like any other. Bytes
// that match one are an intact object written without fs-verity, which is the
// store-mode mismatch and an error.
func (s *Store) CheckObject(ctx context.Context, e Entry) (ObjectCheck, error) {
	path, viewFsv, err := s.entryPath(e)
	if err != nil {
		return ObjectCheck{}, err
	}
	mode, err := s.Mode()
	if err != nil {
		return ObjectCheck{}, err
	}
	f, err := checkOpen(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if IsCorruption(err) {
			return ObjectCheck{Status: ObjectDamaged, Detail: "open: " + err.Error(), Unopenable: true}, nil
		}
		return ObjectCheck{}, fmt.Errorf("objectstore: check %s: %w", path, openError(err))
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("objectstore: failed to close object", "path", path, "err", err)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return ObjectCheck{}, fmt.Errorf("objectstore: check %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return ObjectCheck{}, fmt.Errorf("objectstore: check %s: not a regular file", path)
	}
	// Ask the kernel to drop the file's cached pages so the read reaches the
	// disk. This is advice: pages in use elsewhere stay cached.
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil { // #nosec G115 -- a file descriptor always fits in int
		slog.Debug("objectstore: fadvise failed", "path", path, "err", err)
	}

	h := sha256.New()
	r := &ctxReader{ctx: ctx, r: checkReader(f)}
	var fsv string
	// plain is the failed measurement of an object that carries no fs-verity
	// where the store runs with it.
	var plain error
	if mode == verity.ModeOff {
		fsv, err = verity.Digest(ctx, io.TeeReader(r, h))
	} else {
		fsv, err = verity.Measure(f)
		switch {
		case noVerity(err):
			plain = err
			fsv, err = verity.Digest(ctx, io.TeeReader(r, h))
		case err != nil && !IsCorruption(err):
			return ObjectCheck{}, fmt.Errorf("objectstore: check %s: %w", path, err)
		case err == nil:
			_, err = io.Copy(h, r)
		}
	}
	if err != nil {
		if IsCorruption(err) {
			return ObjectCheck{Status: ObjectDamaged, Detail: "read: " + err.Error(), file: fi}, nil
		}
		return ObjectCheck{}, fmt.Errorf("objectstore: check %s: %w", path, err)
	}
	c := ObjectCheck{Digest: digest.NewDigest(digest.SHA256, h), Fsverity: fsv, file: fi}

	hasCAS, hasView := e.Digest != "", e.ViewName != ""
	if !hasCAS {
		// The inventory may have walked the object tree before this object
		// was committed: look its content-addressed name up now.
		hasCAS = namesInode(fi, s.objectPath(c.Digest.Hex()))
		if hasCAS {
			e.Digest = c.Digest
		}
	}
	casOK := hasCAS && e.Digest == c.Digest
	viewOK := hasView && viewFsv == c.Fsverity
	if plain != nil {
		if casOK || viewOK {
			return ObjectCheck{}, fmt.Errorf("objectstore: check %s: %w", path, requireVerity(plain))
		}
		c.Status, c.Detail = ObjectDamaged, fmt.Sprintf("carries no fs-verity, and its content has digest %s, which matches no name of the object", c.Digest)
		return c, nil
	}
	switch {
	case casOK && (viewOK || !hasView):
		c.Status = ObjectOK
	case casOK:
		c.Status, c.Detail = ObjectMisnamed, fmt.Sprintf("view name %s does not match the fs-verity digest %s", e.ViewName, c.Fsverity)
	case viewOK && !hasCAS:
		c.Status, c.Detail = ObjectMisnamed, "the content-addressed name is missing"
	case viewOK:
		c.Status, c.Detail = ObjectMisnamed, fmt.Sprintf("content-addressed name %s does not match the content digest %s", e.Digest, c.Digest)
	default:
		c.Status, c.Detail = ObjectDamaged, fmt.Sprintf("content has digest %s and fs-verity digest %s, which match no name of the object", c.Digest, c.Fsverity)
	}
	return c, nil
}

// ErrNameTaken reports that RepairNames could not give an object a name its
// bytes call for because another object holds it.
var ErrNameTaken = errors.New("the name is held by another object")

// ErrChanged reports that a name of an object no longer names the inode that
// was checked, so RepairNames or RemoveEntry left that name alone.
var ErrChanged = errors.New("the object changed since it was checked")

// RepairNames gives the misnamed object e, whose check was c, the name its
// bytes call for and it lacks, then removes the name that does not match
// them. It also links the view name of an intact object that has its
// content-addressed name only. The correct name is linked before the wrong
// one is removed, so the object is never left without a name, and a name that
// matches the bytes is never removed. When another object holds the correct
// name, RepairNames changes nothing and returns an error matching
// ErrNameTaken: which of the two objects should keep the name is not decided
// here. A name of e that no longer names the inode c was read from is not
// touched, and the error then matches ErrChanged.
func (s *Store) RepairNames(e Entry, c ObjectCheck) error {
	if c.Status == ObjectDamaged || c.Digest == "" || c.file == nil {
		return errors.New("objectstore: repair names: the object's content is not intact")
	}
	path, viewFsv, err := s.entryPath(e)
	if err != nil {
		return err
	}
	casWant := s.objectPath(c.Digest.Hex())
	viewWant := s.viewPath(strings.TrimPrefix(c.Fsverity, "sha256:"))
	// One name matches the bytes, or the object would be damaged; the other
	// is the one to link, and e's name in that tree, if any, the wrong one.
	var src, want, wrong string
	switch {
	case e.Digest == c.Digest:
		src, want = casWant, viewWant
		if e.ViewName != "" && viewFsv != c.Fsverity {
			wrong = s.viewPath(strings.TrimPrefix(viewFsv, "sha256:"))
		}
	case e.ViewName != "" && viewFsv == c.Fsverity:
		src, want = viewWant, casWant
		if e.Digest != "" {
			wrong = s.objectPath(e.Digest.Hex())
		}
	default:
		return fmt.Errorf("objectstore: repair names: no name of %s matches its content", path)
	}
	if !namesInode(c.file, src) {
		return fmt.Errorf("objectstore: repair names: %s: %w", src, ErrChanged)
	}
	taken, err := linkNoReplace(src, want)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("objectstore: repair names: %s: %w", want, ErrNameTaken)
	}
	if wrong != "" {
		return removeNameOf(c.file, wrong)
	}
	return nil
}

// RemoveEntry unlinks the names the object e has, view name first, unless a
// mounter holds the shared in-use lock on it, in which case it returns an
// error matching ErrInUse. c is the check of e: only a name that still names
// the inode that was read is unlinked, and a name that now names another
// object is left, with an error matching ErrChanged once the others are
// gone. The object is opened through whichever name still names it, so one
// that has a single name is removed too. An object none of whose names
// remain removes nothing and succeeds.
func (s *Store) RemoveEntry(e Entry, c ObjectCheck) error {
	if c.file == nil {
		return errors.New("objectstore: remove object: it was not opened by its check")
	}
	_, viewFsv, err := s.entryPath(e)
	if err != nil {
		return err
	}
	// View name first, so an interrupted removal leaves the name gc and a
	// later removal find the object by.
	var paths []string
	if e.ViewName != "" {
		paths = append(paths, s.viewPath(strings.TrimPrefix(viewFsv, "sha256:")))
	}
	if e.Digest != "" {
		paths = append(paths, s.objectPath(e.Digest.Hex()))
	}
	var own []string
	changed := false
	for _, path := range paths {
		switch _, err := os.Lstat(path); {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("objectstore: stat %s: %w", path, err)
		case namesInode(c.file, path):
			own = append(own, path)
		default:
			changed = true
		}
	}
	if len(own) > 0 {
		f, err := lockUnused(c.file, own[len(own)-1])
		if err != nil {
			return err
		}
		defer func() {
			if err := f.Close(); err != nil {
				slog.Warn("objectstore: failed to close object", "path", f.Name(), "err", err)
			}
		}()
	}
	for _, path := range own {
		if err := removeNameOf(c.file, path); err != nil {
			return err
		}
	}
	if changed {
		return fmt.Errorf("objectstore: remove object: a name of it: %w", ErrChanged)
	}
	return nil
}

// lockUnused opens the object at path, confirms it is the inode file
// describes, and takes the exclusive lock on it without blocking, which
// fails with an error matching ErrInUse while a mounter holds the shared one.
// Closing the returned file drops the lock.
func lockUnused(file fs.FileInfo, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0) // #nosec G304 -- store-derived object path
	if err != nil {
		return nil, fmt.Errorf("objectstore: open object %s: %w", path, openError(err))
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
		err = fmt.Errorf("objectstore: stat object %s: %w", path, err)
	case !os.SameFile(file, fi):
		err = fmt.Errorf("objectstore: object %s: %w", path, ErrChanged)
	default:
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- a file descriptor always fits in int
		if errors.Is(err, unix.EWOULDBLOCK) {
			err = fmt.Errorf("objectstore: object %s: %w", path, ErrInUse)
		} else if err != nil {
			err = fmt.Errorf("objectstore: lock object %s: %w", path, err)
		}
	}
	if err != nil {
		if cerr := f.Close(); cerr != nil {
			slog.Warn("objectstore: failed to close object", "path", path, "err", cerr)
		}
		return nil, err
	}
	return f, nil
}

// EntryInUse reports whether a mounter holds the shared in-use lock on the
// object e, as InUse does, reaching the object through whichever of its
// names it has. It also returns the path it probed, for reading an object
// found in use.
func (s *Store) EntryInUse(e Entry) (inUse bool, path string, err error) {
	path, _, err = s.entryPath(e)
	if err != nil {
		return false, "", err
	}
	inUse, err = inUsePath(path)
	return inUse, path, err
}

// entryPath returns the path CheckObject and RemoveEntry open e through, its
// content-addressed name when it has one and its view name otherwise, and
// the fs-verity digest its view name carries, empty without one.
func (s *Store) entryPath(e Entry) (path, viewFsv string, err error) {
	if e.ViewName != "" {
		if viewFsv, err = viewNameDigest(e.Digest, e.ViewName); err != nil {
			return "", "", err
		}
		path = s.viewPath(strings.TrimPrefix(viewFsv, "sha256:"))
	}
	if e.Digest != "" {
		if path, err = s.ObjectPath(e.Digest); err != nil {
			return "", "", err
		}
	}
	if path == "" {
		return "", "", errors.New("objectstore: object entry has no name")
	}
	return path, viewFsv, nil
}

// linkNoReplace hardlinks src at dst unless dst exists. It reports taken when
// dst exists as another inode; dst already naming src's inode is success.
func linkNoReplace(src, dst string) (taken bool, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- ocicfs-owned store shard dir
		return false, fmt.Errorf("objectstore: create shard dir: %w", err)
	}
	err = os.Link(src, dst)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("objectstore: link %s: %w", dst, err)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return false, fmt.Errorf("objectstore: stat %s: %w", src, err)
	}
	return !namesInode(fi, dst), nil
}

// namesInode reports whether path names the inode fi describes. A missing or
// unreadable path is not the same inode.
func namesInode(fi fs.FileInfo, path string) bool {
	other, err := os.Lstat(path)
	return err == nil && os.SameFile(fi, other)
}

// removeNameOf unlinks path if it names the inode file describes. A name
// already gone is success; one that names another inode is left, with an
// error matching ErrChanged.
func removeNameOf(file fs.FileInfo, path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("objectstore: stat %s: %w", path, err)
	}
	if !os.SameFile(file, fi) {
		return fmt.Errorf("objectstore: remove %s: %w", path, ErrChanged)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("objectstore: remove %s: %w", path, err)
	}
	return nil
}

// IsCorruption reports whether err from opening or reading an object is
// evidence that its stored bytes are bad: EIO is what a read returns when
// fs-verity rejects a block or the device fails, and EBADMSG and EUCLEAN are
// what a filesystem returns for a failed checksum and for corrupt metadata,
// such as an fs-verity descriptor it cannot locate.
func IsCorruption(err error) bool {
	return errors.Is(err, unix.EIO) || errors.Is(err, unix.EBADMSG) || errors.Is(err, unix.EUCLEAN)
}
