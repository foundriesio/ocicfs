// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding.json")
	if err := WriteFile(path, []byte("first"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, []byte("first")) {
		t.Fatalf("content = %q, want first", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 0640", fi.Mode().Perm())
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ref.json")
	if err := WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFile first: %v", err)
	}
	if err := WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("WriteFile second: %v", err)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- test reads a path it wrote
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want second", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestWriteFileRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".knarr-*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temps: %v", matches)
	}
}

func TestMkdirAll(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images", "sha256", "aa", "rest")
	if err := MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}
