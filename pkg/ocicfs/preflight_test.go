// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/verity"
)

func TestPreflightToolsReportsMissingMkcomposefs(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.PreflightTools()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mkcomposefs") {
		t.Fatalf("error = %v, want mkcomposefs message", err)
	}
}

func TestPreflightStorePassesOnVerityCapableStore(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := eng.PreflightStore(); err != nil {
		t.Fatalf("PreflightStore: %v", err)
	}
	// The store probe must not depend on the tools check: a host without
	// mkcomposefs still reports its filesystem accurately.
	t.Setenv("PATH", t.TempDir())
	if err := eng.PreflightStore(); err != nil {
		t.Fatalf("PreflightStore without tools on PATH: %v", err)
	}
	want := "enabled"
	if verity.Mode == verity.ModeOff {
		want = "disabled"
	}
	if state, err := eng.VerityState(); err != nil || state != want {
		t.Fatalf("VerityState = %q, %v; want %s", state, err, want)
	}
}

// TestPreflightStoreWithoutFsverity pins the preflight on a filesystem
// without fs-verity: a build made in required mode fails it with the probe
// error, the others pass and run the store without fs-verity.
func TestPreflightStoreWithoutFsverity(t *testing.T) {
	storeRoot := plainStoreRoot(t)
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.PreflightStore()
	if verity.Mode == verity.ModeRequired {
		if err == nil || !strings.Contains(err.Error(), "fs-verity-capable filesystem") {
			t.Fatalf("PreflightStore = %v, want the probe error", err)
		}
	} else if err != nil {
		t.Fatalf("PreflightStore: %v", err)
	}
	entries, err := os.ReadDir(storeRoot + "/tmp")
	if err != nil || len(entries) != 0 {
		t.Fatalf("store temp dir after the preflight = %d entries, %v; want none", len(entries), err)
	}
}
