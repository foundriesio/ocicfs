// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package holdstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

func TestHoldReleaseIdempotent(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		if err := s.Hold(root, "cli"); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		// A second Hold of the same holder is a no-op success.
		if err := s.Hold(root, "cli"); err != nil {
			t.Fatalf("Hold again: %v", err)
		}
		if holders, err := s.Holders(root); err != nil || len(holders) != 1 || holders[0] != "cli" {
			t.Fatalf("Holders = %v, %v, want [cli]", holders, err)
		}
		if err := s.Release(root, "cli"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		// A second Release of an absent holder is a no-op success.
		if err := s.Release(root, "cli"); err != nil {
			t.Fatalf("Release again: %v", err)
		}
		if holders, err := s.Holders(root); err != nil || len(holders) != 0 {
			t.Fatalf("Holders after release = %v, %v, want empty", holders, err)
		}
	})

	// The last release removes the digest's holder directory.
	dir, err := s.holderDir(root)
	if err != nil {
		t.Fatalf("holderDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("holder dir stat err = %v, want not-exist after last release", err)
	}
}

func TestHoldWritesOnlyTheMarker(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		if err := s.Hold(root, "cli"); err != nil {
			t.Fatalf("Hold: %v", err)
		}
	})

	dir, err := s.holderDir(root)
	if err != nil {
		t.Fatalf("holderDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	// Exactly the holder marker, with no temp artifact a crash could strand and
	// Holders would miscount as a phantom holder.
	if len(entries) != 1 || entries[0].Name() != "cli" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("holder dir entries = %v, want exactly [cli]", names)
	}
}

func TestTwoHoldersIndependent(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		if err := s.Hold(root, "cli"); err != nil {
			t.Fatalf("Hold cli: %v", err)
		}
		if err := s.Hold(root, "cfs"); err != nil {
			t.Fatalf("Hold cfs: %v", err)
		}
		// Releasing one holder leaves the other; the directory survives.
		if err := s.Release(root, "cli"); err != nil {
			t.Fatalf("Release cli: %v", err)
		}
		if holders, err := s.Holders(root); err != nil || len(holders) != 1 || holders[0] != "cfs" {
			t.Fatalf("Holders = %v, %v, want [cfs]", holders, err)
		}
	})

	dir, err := s.holderDir(root)
	if err != nil {
		t.Fatalf("holderDir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("holder dir removed while a holder remains: %v", err)
	}
}

func TestHoldersSorted(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		for _, h := range []string{"zeta", "alpha", "mid"} {
			if err := s.Hold(root, h); err != nil {
				t.Fatalf("Hold %s: %v", h, err)
			}
		}
		holders, err := s.Holders(root)
		if err != nil {
			t.Fatalf("Holders: %v", err)
		}
		want := []string{"alpha", "mid", "zeta"}
		if len(holders) != len(want) {
			t.Fatalf("Holders = %v, want %v", holders, want)
		}
		for i := range want {
			if holders[i] != want[i] {
				t.Fatalf("Holders = %v, want sorted %v", holders, want)
			}
		}
	})
}

func TestClearRemovesAllHolders(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		for _, h := range []string{"cli", "cfs", "agent"} {
			if err := s.Hold(root, h); err != nil {
				t.Fatalf("Hold %s: %v", h, err)
			}
		}
		if err := s.Clear(root); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		if holders, err := s.Holders(root); err != nil || len(holders) != 0 {
			t.Fatalf("Holders after clear = %v, %v, want empty", holders, err)
		}
		// Clearing a digest with no holders is a no-op success.
		if err := s.Clear(root); err != nil {
			t.Fatalf("Clear again: %v", err)
		}
	})
}

func TestBadHolderName(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		for _, bad := range []string{"", ".", "..", "a/b", "UPPER", "sp ace", "sha:256", "emoé"} {
			if err := s.Hold(root, bad); err == nil {
				t.Errorf("Hold(%q): got nil error, want rejection", bad)
			}
			if err := s.Release(root, bad); err == nil {
				t.Errorf("Release(%q): got nil error, want rejection", bad)
			}
		}
	})
}

func TestLockFileSurvivesLastRelease(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	withLock(t, s, root, func() {
		if err := s.Hold(root, "cli"); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if err := s.Release(root, "cli"); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})

	// The lock file lives outside the holder directory and is never removed, so
	// a last-release rmdir cannot invalidate a lock a concurrent caller holds.
	lockPath, err := s.lockPath(root)
	if err != nil {
		t.Fatalf("lockPath: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file stat err = %v, want it to survive the last release", err)
	}
	// It is not inside the holder directory that last-release removes.
	dir, err := s.holderDir(root)
	if err != nil {
		t.Fatalf("holderDir: %v", err)
	}
	if strings.HasPrefix(lockPath, dir+string(filepath.Separator)) {
		t.Fatalf("lock file %s lives under the holder dir %s", lockPath, dir)
	}
}

// TestLockSerializes proves Lock actually blocks: a second acquisition on a
// held digest must not succeed until the first is released. Cross-process
// exclusion relies on flock's per-open-file-description semantics, which the
// kernel enforces; this exercises the same blocking wait in-process with two
// distinct descriptors on one lock file.
func TestLockSerializes(t *testing.T) {
	s := newStore(t)
	root := digest.SHA256.FromString("root")

	unlock, err := s.Lock(root)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	attempting := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(attempting) // about to block on the contended second Lock
		unlock2, err := s.Lock(root)
		if err != nil {
			t.Errorf("second Lock: %v", err)
			close(acquired)
			return
		}
		unlock2()
		close(acquired)
	}()

	<-attempting
	// While the first lock is held the second must block.
	select {
	case <-acquired:
		t.Fatal("second Lock acquired while the first was held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	// Released now, the second must acquire.
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not acquire after the first was released")
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func withLock(t *testing.T, s *Store, root digest.Digest, fn func()) {
	t.Helper()
	unlock, err := s.Lock(root)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()
	fn()
}
