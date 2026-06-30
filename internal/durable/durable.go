// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package durable provides small crash-safe filesystem primitives for metadata
// files that act as store state markers.
package durable

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// WriteFile atomically and durably writes data to path using a temp file in the
// same directory, then fsyncs the parent directory after rename. It takes no
// context deliberately: the write is the atomic commit point of a store
// mutation, and cancelling between fsync and rename would leave exactly the
// partial state this package exists to prevent.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("durable: empty path")
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".knarr-*") // #nosec G304 -- caller-selected metadata directory
	if err != nil {
		return fmt.Errorf("durable: create temp: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("durable: failed to remove temp file", "path", tmp, "err", err)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("durable: write temp: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("durable: chmod temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("durable: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("durable: close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("durable: rename %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("durable: fsync parent: %w", err)
	}
	return nil
}

// MkdirAll creates dir and any missing ancestors, fsyncing the parent of each
// directory it creates.
func MkdirAll(dir string, perm os.FileMode) error {
	if dir == "" {
		return errors.New("durable: empty directory")
	}
	dir = filepath.Clean(dir)
	if dir == "." {
		return nil
	}

	var missing []string
	for p := dir; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("durable: %s is not a directory", p)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("durable: stat %s: %w", p, err)
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
	}

	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, perm); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("durable: mkdir %s: %w", p, err)
			}
			fi, statErr := os.Stat(p)
			if statErr != nil {
				return fmt.Errorf("durable: stat %s: %w", p, statErr)
			}
			if !fi.IsDir() {
				return fmt.Errorf("durable: %s is not a directory", p)
			}
			continue
		}
		if err := syncDir(filepath.Dir(p)); err != nil {
			return fmt.Errorf("durable: fsync parent for %s: %w", p, err)
		}
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- caller-selected metadata directory
	if err != nil {
		return err
	}
	defer func() {
		if err := d.Close(); err != nil {
			slog.Warn("durable: failed to close directory", "path", dir, "err", err)
		}
	}()
	return d.Sync()
}
