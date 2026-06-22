// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"errors"
	"testing"
)

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(Config{Root: ""}); err == nil {
		t.Fatal("New with empty root: got nil error, want error")
	}
}

func TestEngineStubReturnsNotImplemented(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	calls := map[string]func() error{
		"Publish": func() error { return eng.Publish(ctx, "dir", "dest") },
		"Pull":    func() error { _, err := eng.Pull(ctx, "ref"); return err },
		"Mount":   func() error { return eng.Mount(ctx, "ref", "target") },
		"Unmount": func() error { return eng.Unmount(ctx, "target") },
		"List":    func() error { _, err := eng.List(ctx); return err },
		"Remove":  func() error { return eng.Remove(ctx, "ref") },
	}

	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s: got %v, want ErrNotImplemented", name, err)
		}
	}
}
