// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/verity"
)

// newPlainStore opens a store on a temp dir whose filesystem cannot enable
// fs-verity (tmpfs or overlayfs), skipping when it unexpectedly can.
func newPlainStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if err := s.CheckVerity(); err == nil {
		t.Skip("temp dir filesystem unexpectedly supports fs-verity")
	}
	return s
}

// TestModeFromBuild asserts the mode is the build's: required and off as
// built, whatever the filesystem; auto by whether the filesystem can enable
// fs-verity, probed once and kept.
func TestModeFromBuild(t *testing.T) {
	tests := []struct {
		name     string
		verityFS bool
		build    verity.Policy
		want     verity.Policy
	}{
		{name: "auto without fs-verity", build: verity.ModeAuto, want: verity.ModeOff},
		{name: "off without fs-verity", build: verity.ModeOff, want: verity.ModeOff},
		// Required needs no probe; the pull's preflight refuses the
		// filesystem before anything is written.
		{name: "required without fs-verity", build: verity.ModeRequired, want: verity.ModeRequired},
		{name: "auto with fs-verity", verityFS: true, build: verity.ModeAuto, want: verity.ModeRequired},
		{name: "off with fs-verity", verityFS: true, build: verity.ModeOff, want: verity.ModeOff},
		{name: "required with fs-verity", verityFS: true, build: verity.ModeRequired, want: verity.ModeRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s *Store
			if tt.verityFS {
				s = newVerityStore(t)
			} else {
				s = newPlainStore(t)
			}
			s.mode = tt.build
			got, err := s.Mode()
			if err != nil || got != tt.want {
				t.Fatalf("Mode = %q, %v; want %q", got, err, tt.want)
			}
			if s.mode != tt.want {
				t.Fatalf("mode kept = %q, want %q", s.mode, tt.want)
			}
			if entries, err := os.ReadDir(s.TempDir()); err != nil || len(entries) != 0 {
				t.Fatalf("temp dir after the probe = %d entries, %v; want none", len(entries), err)
			}
		})
	}
}

// TestAutoModeProbesOnce asserts an auto build probes the filesystem until it
// gets a definite answer and never again: a failed probe is not kept, and
// Preflight after a definite answer, from any number of callers, probes
// nothing.
func TestAutoModeProbesOnce(t *testing.T) {
	if verity.Mode != verity.ModeAuto {
		t.Skip("Preflight probes in a build made in required mode")
	}
	s := newPlainStore(t)
	s.mode = verity.ModeAuto
	tmp := s.TempDir()
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mode(); err == nil {
		t.Fatal("Mode succeeded with no temp dir to probe in")
	}
	if s.mode != verity.ModeAuto {
		t.Fatalf("failed probe kept as %q", s.mode)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Mode(); err != nil || got != verity.ModeOff {
		t.Fatalf("Mode = %q, %v; want %q", got, err, verity.ModeOff)
	}
	// With the temp dir gone again, any further probe would fail.
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Preflight()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Preflight probed again: %v", err)
		}
	}
}

// TestOpenErrorNamesKernelWithoutFsverity asserts an object open refused
// with EOPNOTSUPP names a kernel without fs-verity support and keeps the
// errno, and any other error passes through unchanged.
func TestOpenErrorNamesKernelWithoutFsverity(t *testing.T) {
	err := openError(&os.PathError{Op: "open", Path: "object", Err: unix.EOPNOTSUPP})
	if !errors.Is(err, unix.EOPNOTSUPP) || !strings.Contains(err.Error(), "kernel without fs-verity support") {
		t.Fatalf("openError(EOPNOTSUPP) = %v", err)
	}
	if got := openError(os.ErrNotExist); got != os.ErrNotExist {
		t.Fatalf("openError(ErrNotExist) = %v, want it unchanged", got)
	}
}

// TestOffModeReadsObjectsWithFsverity asserts off mode reads an object a
// required build wrote: its userspace digest equals the kernel's measurement,
// so a colliding write, resolution and the digest all agree with it, and
// removal by that name clears both of the object's names.
func TestOffModeReadsObjectsWithFsverity(t *testing.T) {
	s := newVerityStore(t)
	ctx := context.Background()
	content := bytes.Repeat([]byte("sealed object "), 1000)
	d, fsv, err := writeObject(t, s, ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	s.mode = verity.ModeOff

	if _, again, err := writeObject(t, s, ctx, bytes.NewReader(content)); err != nil || again != fsv {
		t.Fatalf("colliding write = %s, %v; want %s", again, err, fsv)
	}
	if got, size, err := s.ResolveObject(ctx, d, ""); err != nil || got != fsv || size != int64(len(content)) {
		t.Fatalf("ResolveObject = %s, %d, %v; want %s, %d", got, size, err, fsv, len(content))
	}
	if got, err := s.VerityDigest(ctx, d); err != nil || got != fsv {
		t.Fatalf("VerityDigest = %s, %v; want %s", got, err, fsv)
	}
	if err := s.Remove(d, viewName(fsv)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	objPath, _ := s.ObjectPath(d)
	for _, p := range []string{objPath, filepath.Join(s.ViewDir(), viewName(fsv))} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still present after Remove: %v", p, err)
		}
	}
}

// TestOffModeObjects drives the object paths of off mode on a filesystem
// that cannot enable fs-verity: writes, collisions, resolution, the digest
// and removal all work from userspace digests equal to the kernel's.
func TestOffModeObjects(t *testing.T) {
	s := newPlainStore(t)
	s.mode = verity.ModeOff
	ctx := context.Background()
	content := bytes.Repeat([]byte("off-mode object "), 1000)
	want, err := verity.Digest(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	d, fsv, err := writeObject(t, s, ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	if fsv != want {
		t.Fatalf("WriteObject fs-verity digest = %s, want %s", fsv, want)
	}
	// A second write of the same bytes resolves the existing object.
	if _, again, err := writeObject(t, s, ctx, bytes.NewReader(content)); err != nil || again != want {
		t.Fatalf("rewrite = %s, %v; want %s", again, err, want)
	}
	if got, err := s.VerityDigest(ctx, d); err != nil || got != want {
		t.Fatalf("VerityDigest = %s, %v; want %s", got, err, want)
	}
	if got, size, err := s.ResolveObject(ctx, d, ""); err != nil || got != want || size != int64(len(content)) {
		t.Fatalf("ResolveObject = %s, %d, %v; want %s, %d", got, size, err, want, len(content))
	}
	// A blob goes through the same path.
	if err := s.WriteBlob(ctx, d, bytes.NewReader(content)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	viewPath := filepath.Join(s.ViewDir(), viewName(want))
	if _, err := os.Lstat(viewPath); err != nil {
		t.Fatalf("view name missing: %v", err)
	}
	// Given the view name, ResolveObject reads no content: a corrupted
	// object of the same size still resolves to the name.
	objPath, _ := s.ObjectPath(d)
	if err := os.WriteFile(objPath, bytes.Repeat([]byte("x"), len(content)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, size, err := s.ResolveObject(ctx, d, viewName(want)); err != nil || got != want || size != int64(len(content)) {
		t.Fatalf("ResolveObject with the view name = %s, %d, %v; want %s, %d", got, size, err, want, len(content))
	}
	if err := s.Remove(d, "zz/not-a-name"); err == nil {
		t.Fatal("Remove accepted a malformed view name")
	}
	if err := s.Remove(d, viewName(want)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, p := range []string{objPath, viewPath} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still present after Remove: %v", p, err)
		}
	}
}

// TestOffModeWriterSealsInUserspace asserts the background writer gives an
// object in off mode the userspace digest, enabling nothing, even on a
// filesystem that supports fs-verity.
func TestOffModeWriterSealsInUserspace(t *testing.T) {
	s := newVerityStore(t)
	s.mode = verity.ModeOff
	ctx := context.Background()
	content := []byte("written by the background writer in off mode")
	w := s.NewWriter(ctx)
	d, sealed, err := w.WriteObject(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	fsv, err := sealed()
	if err != nil {
		t.Fatalf("sealed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want, err := verity.Digest(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if fsv != want {
		t.Fatalf("fs-verity digest = %s, want %s", fsv, want)
	}
	path, _ := s.ObjectPath(d)
	f, err := os.Open(path) // #nosec G304 -- test store object
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := verity.Measure(f); err == nil {
		t.Fatal("an object written in off mode has fs-verity enabled")
	}
}
