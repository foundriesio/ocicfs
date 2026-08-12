// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/foundriesio/knarr/internal/devtest"
)

func TestPreflightReportsMissingMkcomposefs(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.Preflight()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mkcomposefs") {
		t.Fatalf("error = %v, want mkcomposefs message", err)
	}
}

func TestPreflightPassesOnVerityCapableStore(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
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
	if err := eng.Preflight(); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}
