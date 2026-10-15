// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/foundriesio/ocicfs/internal/devtest"
)

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(Config{Root: ""}); err == nil {
		t.Fatal("New with empty root: got nil error, want error")
	}
}

func TestNewIngestWorkers(t *testing.T) {
	if _, err := New(Config{Root: t.TempDir(), IngestWorkers: -1}); err == nil || !strings.Contains(err.Error(), "ingest workers must not be negative") {
		t.Fatalf("New with IngestWorkers -1: got %v, want a negative-workers error", err)
	}
	for _, tt := range []struct {
		workers, wantCap int
	}{
		{0, DefaultIngestWorkers},
		{1, 1},
		{4, 4},
	} {
		eng, err := New(Config{Root: t.TempDir(), IngestWorkers: tt.workers})
		if err != nil {
			t.Fatalf("New with IngestWorkers %d: %v", tt.workers, err)
		}
		if got := cap(eng.(*engine).slots); got != tt.wantCap {
			t.Errorf("IngestWorkers %d: limiter cap = %d, want %d", tt.workers, got, tt.wantCap)
		}
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
	}

	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s: got %v, want ErrNotImplemented", name, err)
		}
	}
}

func TestMountRejectsWritableOptions(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	target := t.TempDir()
	ref := "app@" + digest.SHA256.FromString("root").String()
	tests := []struct {
		name string
		opt  MountOption
	}{
		{"writable", WithWritable()},
		{"overlay root", func(o *MountOptions) { o.OverlayRoot = "/overlays" }},
		{"upper dir", func(o *MountOptions) { o.UpperDir = "/upper" }},
		{"work dir", func(o *MountOptions) { o.WorkDir = "/work" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := eng.Mount(context.Background(), ref, target, tt.opt)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), writableMountUnsupported) {
				t.Fatalf("error = %v, want writable unsupported", err)
			}
		})
	}
}

func TestMountValidatesRefAndTargetBeforeStore(t *testing.T) {
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	target := t.TempDir()
	if err := eng.Mount(context.Background(), "example.com/app:latest", target); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not recorded by a pull") {
		t.Fatalf("unrecorded tag mount error = %v, want ErrNotFound with a pull hint", err)
	}
	if err := eng.Mount(context.Background(), "example.com/app@sha256:nothex", target); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "invalid digest") {
		t.Fatalf("malformed pinned-ref mount error = %v, want a digest parse error, not ErrNotFound", err)
	}

	ref := "app@" + digest.SHA256.FromString("root").String()
	missing := target + "-missing"
	if err := eng.Mount(context.Background(), ref, missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing target error = %v", err)
	}

	file, err := os.CreateTemp(t.TempDir(), "target-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := eng.Mount(context.Background(), ref, path); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file target error = %v", err)
	}
}

func TestMountMissingBindingReportsPullFirst(t *testing.T) {
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
	ref := "app@" + digest.SHA256.FromString("root").String()
	err = eng.Mount(context.Background(), ref, t.TempDir())
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not pulled for this platform") || !strings.Contains(err.Error(), "ocicfs pull") {
		t.Fatalf("error = %v, want pull-first message", err)
	}
}
