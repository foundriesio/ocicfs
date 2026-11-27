// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/verity"
)

// eachMode runs fn against a store without fs-verity, on a plain temp dir,
// and against one with it, on the dev container's verity-capable filesystem;
// the second is skipped where that filesystem is unavailable.
func eachMode(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("off", func(t *testing.T) {
		s := newTestStore(t)
		s.mode = verity.ModeOff
		fn(t, s)
	})
	t.Run("required", func(t *testing.T) {
		fn(t, newVerityStore(t))
	})
}

// stored writes content into s and returns its entry and its two paths.
func stored(t *testing.T, s *Store, content string) (e Entry, objPath, viewPath string) {
	t.Helper()
	d, fsv, err := writeObject(t, s, context.Background(), strings.NewReader(content))
	skipIfNoVerity(t, err)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	objPath, err = s.ObjectPath(d)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	return Entry{Digest: d, ViewName: viewName(fsv)}, objPath, filepath.Join(s.ViewDir(), viewName(fsv))
}

// otherDigest is a valid sha256 digest no test object has.
var otherDigest = digest.FromString("some other content")

func otherViewName() string {
	return otherDigest.Hex()[:2] + "/" + otherDigest.Hex()[2:]
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, s *Store, e Entry) ObjectCheck {
	t.Helper()
	c, err := s.CheckObject(context.Background(), e)
	if err != nil {
		t.Fatalf("CheckObject(%+v): %v", e, err)
	}
	return c
}

func present(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// failReads makes CheckObject's reads fail with err for the test's duration.
func failReads(t *testing.T, err error) {
	t.Helper()
	orig := checkReader
	checkReader = func(*os.File) io.Reader { return errReader{err} }
	t.Cleanup(func() { checkReader = orig })
}

// failOpens makes CheckObject's opens fail with err for the test's duration.
func failOpens(t *testing.T, err error) {
	t.Helper()
	orig := checkOpen
	checkOpen = func(name string, _ int, _ os.FileMode) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	t.Cleanup(func() { checkOpen = orig })
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestCheckObjectNames(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		e, objPath, viewPath := stored(t, s, "both names")
		if c := check(t, s, e); c.Status != ObjectOK || c.Digest != e.Digest || viewName(c.Fsverity) != e.ViewName {
			t.Fatalf("intact object: %+v", c)
		}
		// A view-only entry whose content-addressed name exists after all,
		// as when the object was committed between the inventory walks.
		if c := check(t, s, Entry{ViewName: e.ViewName}); c.Status != ObjectOK {
			t.Fatalf("view entry of a paired object: %+v", c)
		}

		// Content-addressed name only: intact, and the check names the view
		// name it should have.
		only, _, onlyView := stored(t, s, "content-addressed name only")
		mustRemove(t, onlyView)
		if c := check(t, s, Entry{Digest: only.Digest}); c.Status != ObjectOK || viewName(c.Fsverity) != only.ViewName {
			t.Fatalf("content-addressed-only object: %+v", c)
		}

		// View name only: the content is intact, a name is lost.
		lost, lostObj, _ := stored(t, s, "view name only")
		mustRemove(t, lostObj)
		if c := check(t, s, Entry{ViewName: lost.ViewName}); c.Status != ObjectMisnamed || c.Digest != lost.Digest {
			t.Fatalf("view-only object: %+v", c)
		}

		// A wrong content-addressed name over intact bytes.
		mustRename(t, objPath, s.objectPath(otherDigest.Hex()))
		if c := check(t, s, Entry{Digest: otherDigest, ViewName: e.ViewName}); c.Status != ObjectMisnamed || c.Digest != e.Digest {
			t.Fatalf("wrong content-addressed name: %+v", c)
		}
		mustRename(t, s.objectPath(otherDigest.Hex()), objPath)

		// A wrong view name over intact bytes.
		mustRename(t, viewPath, s.viewPath(otherDigest.Hex()))
		if c := check(t, s, Entry{Digest: e.Digest, ViewName: otherViewName()}); c.Status != ObjectMisnamed || viewName(c.Fsverity) != e.ViewName {
			t.Fatalf("wrong view name: %+v", c)
		}

		// No name matches: a view-only object under a wrong view name, and
		// a content-addressed-only one under a wrong digest.
		mustRemove(t, objPath)
		if c := check(t, s, Entry{ViewName: otherViewName()}); c.Status != ObjectDamaged {
			t.Fatalf("view-only object under a wrong name: %+v", c)
		}
		mustRename(t, s.viewPath(otherDigest.Hex()), s.objectPath(otherDigest.Hex()))
		if c := check(t, s, Entry{Digest: otherDigest}); c.Status != ObjectDamaged {
			t.Fatalf("content-addressed-only object under a wrong name: %+v", c)
		}
	})
}

// Without fs-verity nothing stops an object's bytes from changing on disk.
func TestCheckObjectChangedBytes(t *testing.T) {
	s := newTestStore(t)
	s.mode = verity.ModeOff
	e, objPath, _ := stored(t, s, "original bytes")
	if err := os.WriteFile(objPath, []byte("changed bytes!"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []Entry{e, {Digest: e.Digest}, {ViewName: e.ViewName}} {
		if c := check(t, s, entry); c.Status != ObjectDamaged {
			t.Fatalf("changed bytes under %+v: %+v", entry, c)
		}
	}
}

func TestCheckObjectCorruptionErrors(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		e, _, _ := stored(t, s, "unreadable")
		for _, errno := range []error{unix.EIO, unix.EBADMSG, unix.EUCLEAN} {
			t.Run(errno.Error(), func(t *testing.T) {
				failReads(t, errno)
				if c := check(t, s, e); c.Status != ObjectDamaged || c.Unopenable {
					t.Fatalf("read failing with %v: %+v", errno, c)
				}
				failOpens(t, errno)
				if c := check(t, s, e); c.Status != ObjectDamaged || !c.Unopenable {
					t.Fatalf("open failing with %v: %+v", errno, c)
				}
			})
		}
	})
}

// A failure that says nothing about the bytes must stop the check, never
// condemn the object.
func TestCheckObjectOperationalErrors(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		e, _, _ := stored(t, s, "intact")
		for _, errno := range []error{unix.EACCES, unix.EMFILE, unix.ENOMEM, unix.EOPNOTSUPP, unix.EINVAL} {
			t.Run(errno.Error(), func(t *testing.T) {
				failReads(t, errno)
				if c, err := s.CheckObject(context.Background(), e); !errors.Is(err, errno) {
					t.Fatalf("read failing with %v: check %+v, err %v", errno, c, err)
				}
				failOpens(t, errno)
				if c, err := s.CheckObject(context.Background(), e); !errors.Is(err, errno) {
					t.Fatalf("open failing with %v: check %+v, err %v", errno, c, err)
				}
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if c, err := s.CheckObject(ctx, e); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled check: %+v, err %v", c, err)
		}
	})
}

// A file without fs-verity put in the place of an object's content-addressed
// name is damaged, and removing it lets the object, intact under its view
// name, have that name back.
func TestCheckObjectReplacedByPlainFile(t *testing.T) {
	s := newVerityStore(t)
	e, objPath, _ := stored(t, s, "the object")
	mustRemove(t, objPath)
	if err := os.WriteFile(objPath, []byte("something else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fake, original := Entry{Digest: e.Digest}, Entry{ViewName: e.ViewName}
	c, err := s.CheckObject(ctx, fake)
	if err != nil || c.Status != ObjectDamaged || !strings.Contains(c.Detail, "carries no fs-verity") {
		t.Fatalf("check of the plain file = %+v, err %v; want damaged", c, err)
	}
	oc, err := s.CheckObject(ctx, original)
	if err != nil || oc.Status != ObjectMisnamed {
		t.Fatalf("check of the object = %+v, err %v; want misnamed", oc, err)
	}
	if err := s.RemoveEntry(fake, c); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if err := s.RepairNames(original, oc); err != nil {
		t.Fatalf("RepairNames: %v", err)
	}
	if c, err := s.CheckObject(ctx, e); err != nil || c.Status != ObjectOK {
		t.Fatalf("check after the repair = %+v, err %v", c, err)
	}

	// The same file under a view name only.
	view := Entry{ViewName: viewName(strings.Repeat("ab", 32))}
	path := filepath.Join(s.ViewDir(), view.ViewName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- test store shard dir
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("something else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := s.CheckObject(ctx, view); err != nil || c.Status != ObjectDamaged {
		t.Fatalf("check of the plain view entry = %+v, err %v; want damaged", c, err)
	}
}

// An intact object without fs-verity in a store run with it is the
// store-mode mismatch, not damage.
func TestCheckObjectWithoutVerity(t *testing.T) {
	s := newVerityStore(t)
	s.mode = verity.ModeOff
	e, _, _ := stored(t, s, "written without fs-verity")
	s.mode = verity.ModeRequired
	c, err := s.CheckObject(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "carries no fs-verity") {
		t.Fatalf("check = %+v, err %v; want the no-fs-verity error", c, err)
	}
}

func TestInventory(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		both, _, _ := stored(t, s, "both")
		casOnly, _, casOnlyView := stored(t, s, "content-addressed only")
		mustRemove(t, casOnlyView)
		viewOnly, viewOnlyObj, _ := stored(t, s, "view only")
		mustRemove(t, viewOnlyObj)
		// A second view name on the first object.
		if err := os.MkdirAll(filepath.Dir(s.viewPath(otherDigest.Hex())), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(s.objectPath(both.Digest.Hex()), s.viewPath(otherDigest.Hex())); err != nil {
			t.Fatal(err)
		}

		entries, err := s.Inventory(context.Background())
		if err != nil {
			t.Fatalf("Inventory: %v", err)
		}
		got := map[Entry]bool{}
		for _, e := range entries {
			got[e] = true
		}
		want := []Entry{
			{Digest: casOnly.Digest},
			{ViewName: viewOnly.ViewName},
			{Digest: both.Digest, ViewName: both.ViewName},
			{Digest: both.Digest, ViewName: otherViewName()},
		}
		if len(entries) != len(want) {
			t.Fatalf("Inventory = %+v, want %+v", entries, want)
		}
		for _, e := range want {
			if !got[e] {
				t.Fatalf("Inventory = %+v, missing %+v", entries, e)
			}
		}
	})
}

func TestRemoveEntry(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		remove := func(e Entry) error {
			t.Helper()
			return s.RemoveEntry(e, check(t, s, e))
		}
		both, bothObj, bothView := stored(t, s, "both")
		bothCheck := check(t, s, both)
		if err := s.RemoveEntry(both, bothCheck); err != nil {
			t.Fatalf("RemoveEntry: %v", err)
		}
		if present(bothObj) || present(bothView) {
			t.Fatal("names left after removing an object with both")
		}
		if err := s.RemoveEntry(both, bothCheck); err != nil {
			t.Fatalf("RemoveEntry of an absent object: %v", err)
		}

		// The remnant of a removal interrupted between its two unlinks.
		remnant, remnantObj, remnantView := stored(t, s, "remnant")
		mustRemove(t, remnantView)
		if err := remove(Entry{Digest: remnant.Digest}); err != nil || present(remnantObj) {
			t.Fatalf("RemoveEntry of a content-addressed-only object: %v", err)
		}

		viewOnly, viewOnlyObj, viewOnlyView := stored(t, s, "view only")
		mustRemove(t, viewOnlyObj)
		if err := remove(Entry{ViewName: viewOnly.ViewName}); err != nil || present(viewOnlyView) {
			t.Fatalf("RemoveEntry of a view-only object: %v", err)
		}

		held, heldObj, heldView := stored(t, s, "held by a mount")
		mounter, err := os.Open(heldObj) // #nosec G304 -- store path derived in test
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mounter.Close() }()
		if err := unix.Flock(int(mounter.Fd()), unix.LOCK_SH); err != nil { // #nosec G115 -- a file descriptor always fits in int
			t.Fatal(err)
		}
		if err := remove(held); !errors.Is(err, ErrInUse) {
			t.Fatalf("RemoveEntry = %v, want ErrInUse", err)
		}
		if !present(heldObj) || !present(heldView) {
			t.Fatal("an in-use object lost a name")
		}

		// An object that was not opened by its check is not removed.
		if err := s.RemoveEntry(held, ObjectCheck{Status: ObjectDamaged, Unopenable: true}); err == nil {
			t.Fatal("RemoveEntry accepted an object its check did not open")
		}
	})
}

// A damaged object has two content-addressed names and holds the view name
// an intact object should have. It is listed once per name and both listings
// are checked before anything changes. Removing it through the first frees
// the view name, the intact object's repair takes it, and the removal through
// the second must then leave that name alone.
func TestRemoveEntryLeavesNameRepairedMeanwhile(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		b, _, bView := stored(t, s, "the intact object")
		_, aObj, aView := stored(t, s, "the damaged object")
		// The damaged object takes the intact one's view name and a second,
		// equally wrong, content-addressed name.
		mustRemove(t, aView)
		mustRename(t, bView, s.viewPath(otherDigest.Hex()))
		if err := os.Link(aObj, bView); err != nil {
			t.Fatal(err)
		}
		a1 := digest.FromString("first wrong name")
		a2 := digest.FromString("second wrong name")
		mustRename(t, aObj, s.objectPath(a1.Hex()))
		if err := os.MkdirAll(filepath.Dir(s.objectPath(a2.Hex())), 0o755); err != nil { // #nosec G301 -- test store directory
			t.Fatal(err)
		}
		if err := os.Link(s.objectPath(a1.Hex()), s.objectPath(a2.Hex())); err != nil {
			t.Fatal(err)
		}

		first, second := Entry{Digest: a1, ViewName: b.ViewName}, Entry{Digest: a2, ViewName: b.ViewName}
		intact := Entry{Digest: b.Digest, ViewName: otherViewName()}
		firstCheck, secondCheck, intactCheck := check(t, s, first), check(t, s, second), check(t, s, intact)
		if firstCheck.Status != ObjectDamaged || secondCheck.Status != ObjectDamaged || intactCheck.Status != ObjectMisnamed {
			t.Fatalf("checks = %+v, %+v, %+v", firstCheck, secondCheck, intactCheck)
		}

		if err := s.RemoveEntry(first, firstCheck); err != nil {
			t.Fatalf("RemoveEntry through the first name: %v", err)
		}
		if err := s.RepairNames(intact, intactCheck); err != nil {
			t.Fatalf("RepairNames: %v", err)
		}
		if err := s.RemoveEntry(second, secondCheck); !errors.Is(err, ErrChanged) {
			t.Fatalf("RemoveEntry through the second name = %v, want ErrChanged", err)
		}
		if present(s.objectPath(a1.Hex())) || present(s.objectPath(a2.Hex())) {
			t.Fatal("a name of the damaged object survived")
		}
		if c := check(t, s, b); c.Status != ObjectOK {
			t.Fatalf("the intact object after the removals: %+v", c)
		}
	})
}

// A name that came to name another object since the check is not unlinked by
// a repair either.
func TestRepairNamesLeavesChangedName(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		e, objPath, viewPath := stored(t, s, "repaired after it changed")
		wrong := s.viewPath(otherDigest.Hex())
		mustRename(t, viewPath, wrong)
		misnamed := Entry{Digest: e.Digest, ViewName: otherViewName()}
		c := check(t, s, misnamed)
		// Another object takes over the wrong name.
		_, otherObj, _ := stored(t, s, "another object")
		mustRemove(t, wrong)
		if err := os.Link(otherObj, wrong); err != nil {
			t.Fatal(err)
		}
		if err := s.RepairNames(misnamed, c); !errors.Is(err, ErrChanged) {
			t.Fatalf("RepairNames = %v, want ErrChanged", err)
		}
		if !present(wrong) || !present(objPath) || !present(viewPath) {
			t.Fatal("the repair removed a name of another object, or did not link the right one")
		}
	})
}

func TestRepairNames(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		repair := func(e Entry) error {
			t.Helper()
			return s.RepairNames(e, check(t, s, e))
		}
		intact := func(e Entry) {
			t.Helper()
			if c := check(t, s, e); c.Status != ObjectOK {
				t.Fatalf("after repair %+v: %+v", e, c)
			}
		}

		// A lost content-addressed name is linked again.
		a, aObj, _ := stored(t, s, "lost content-addressed name")
		mustRemove(t, aObj)
		if err := repair(Entry{ViewName: a.ViewName}); err != nil {
			t.Fatalf("repair: %v", err)
		}
		intact(a)

		// A lost view name is linked again.
		b, _, bView := stored(t, s, "lost view name")
		mustRemove(t, bView)
		if err := repair(Entry{Digest: b.Digest}); err != nil {
			t.Fatalf("repair: %v", err)
		}
		intact(b)

		// A wrong content-addressed name is replaced by the right one.
		c, cObj, _ := stored(t, s, "wrong content-addressed name")
		wrongObj := s.objectPath(otherDigest.Hex())
		mustRename(t, cObj, wrongObj)
		if err := repair(Entry{Digest: otherDigest, ViewName: c.ViewName}); err != nil {
			t.Fatalf("repair: %v", err)
		}
		intact(c)
		if present(wrongObj) {
			t.Fatal("the wrong content-addressed name survived the repair")
		}

		// A wrong view name is replaced by the right one.
		d, _, dView := stored(t, s, "wrong view name")
		wrongView := s.viewPath(otherDigest.Hex())
		mustRename(t, dView, wrongView)
		if err := repair(Entry{Digest: d.Digest, ViewName: otherViewName()}); err != nil {
			t.Fatalf("repair: %v", err)
		}
		intact(d)
		if present(wrongView) {
			t.Fatal("the wrong view name survived the repair")
		}

		// A damaged object is not repaired.
		if err := s.RepairNames(a, ObjectCheck{Status: ObjectDamaged}); err == nil {
			t.Fatal("RepairNames accepted a damaged object")
		}
	})
}

// The content-addressed name a view-only object should get is already held
// by a second copy of the same content: the repair changes nothing, whether
// that copy is intact or not.
func TestRepairNamesLeavesTakenName(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		e, objPath, viewPath := stored(t, s, "two copies")
		// Leave the first inode with its view name only, then store the
		// content again: the second inode takes the content-addressed name
		// and cannot take the view name.
		mustRemove(t, objPath)
		mustRename(t, viewPath, viewPath+".first")
		stored(t, s, "two copies")
		mustRemove(t, viewPath)
		mustRename(t, viewPath+".first", viewPath)
		viewFI, err := os.Lstat(viewPath)
		if err != nil {
			t.Fatal(err)
		}

		orphan := Entry{ViewName: e.ViewName}
		c := check(t, s, orphan)
		if c.Status != ObjectMisnamed {
			t.Fatalf("view-only duplicate: %+v", c)
		}
		if err := s.RepairNames(orphan, c); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("RepairNames = %v, want ErrNameTaken", err)
		}
		if !namesInode(viewFI, viewPath) || namesInode(viewFI, objPath) || !present(objPath) {
			t.Fatal("a refused repair changed a name")
		}
		// The other copy lacks the view name the first holds.
		if err := s.RepairNames(Entry{Digest: e.Digest}, check(t, s, Entry{Digest: e.Digest})); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("RepairNames of the second copy = %v, want ErrNameTaken", err)
		}
		if !namesInode(viewFI, viewPath) || !present(objPath) {
			t.Fatal("a refused repair changed a name")
		}
	})
}

// Two intact objects carry each other's view name. Neither repair may take
// the other's name: both stay misnamed and both contents stay readable.
func TestRepairNamesSwappedViewNames(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		a, aObj, aView := stored(t, s, "content of the first object")
		b, bObj, bView := stored(t, s, "content of the second object")
		mustRename(t, aView, aView+".tmp")
		mustRename(t, bView, aView)
		mustRename(t, aView+".tmp", bView)

		for _, e := range []Entry{{Digest: a.Digest, ViewName: b.ViewName}, {Digest: b.Digest, ViewName: a.ViewName}} {
			c := check(t, s, e)
			if c.Status != ObjectMisnamed || c.Digest != e.Digest {
				t.Fatalf("object with a swapped view name: %+v", c)
			}
			if err := s.RepairNames(e, c); !errors.Is(err, ErrNameTaken) {
				t.Fatalf("RepairNames = %v, want ErrNameTaken", err)
			}
		}
		for path, want := range map[string]string{aObj: "content of the first object", bObj: "content of the second object"} {
			got, err := os.ReadFile(path) // #nosec G304 -- store path derived in test
			if err != nil || string(got) != want {
				t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
			}
		}
		if !present(aView) || !present(bView) {
			t.Fatal("a refused repair removed a view name")
		}
	})
}

// The in-use probe reaches an object that has its view name only.
func TestEntryInUse(t *testing.T) {
	eachMode(t, func(t *testing.T, s *Store) {
		e, objPath, viewPath := stored(t, s, "mounted, then lost its content-addressed name")
		mounter, err := os.Open(objPath) // #nosec G304 -- store path derived in test
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mounter.Close() }()
		if err := unix.Flock(int(mounter.Fd()), unix.LOCK_SH); err != nil { // #nosec G115 -- a file descriptor always fits in int
			t.Fatal(err)
		}
		mustRemove(t, objPath)
		inUse, path, err := s.EntryInUse(Entry{ViewName: e.ViewName})
		if err != nil || !inUse || path != viewPath {
			t.Fatalf("EntryInUse = %v, %q, %v", inUse, path, err)
		}
		other, _, _ := stored(t, s, "not mounted")
		if inUse, _, err := s.EntryInUse(other); err != nil || inUse {
			t.Fatalf("EntryInUse of an unlocked object = %v, %v", inUse, err)
		}
	})
}
