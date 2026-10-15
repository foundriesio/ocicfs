// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"errors"
	"strings"
	"testing"

	"github.com/foundriesio/ocicfs/internal/devtest"
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
}
