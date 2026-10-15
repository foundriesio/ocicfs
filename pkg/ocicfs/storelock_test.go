// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestStoreLockExclusiveFailsFastWhileShared pins the lock protocol: gc's
// exclusive acquisition never blocks — it fails with ErrStoreBusy while any
// pull, hold, or mount holds the shared lock, and succeeds once released.
func TestStoreLockExclusiveFailsFastWhileShared(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := eng.(*engine)

	unlockShared, err := e.lockStoreShared()
	if err != nil {
		t.Fatalf("lockStoreShared: %v", err)
	}
	if _, err := e.lockStoreExclusive(); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("lockStoreExclusive under a shared holder = %v, want ErrStoreBusy", err)
	}
	unlockShared()

	unlockExclusive, err := e.lockStoreExclusive()
	if err != nil {
		t.Fatalf("lockStoreExclusive after release: %v", err)
	}
	unlockExclusive()

	// The lock file survives every release; a removed lock file would let a
	// waiter acquire a lock nobody else can see.
	if _, err := os.Stat(filepath.Join(e.root, ".lock")); err != nil {
		t.Fatalf("lock file after release: %v", err)
	}
}
