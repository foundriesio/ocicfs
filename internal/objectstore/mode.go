// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/verity"
)

// Mode returns the fs-verity mode this build writes and reads the store in,
// verity.ModeRequired or verity.ModeOff. A required or off build is its own
// mode. An auto build probes the store's filesystem once, on the first call:
// it is required when the probe enables fs-verity and off when the
// filesystem or kernel cannot; a probe failing for any other reason is
// returned and run again on the next call. Nothing reads the mode from the
// objects: an object without fs-verity met in required mode fails where it
// is measured.
func (s *Store) Mode() (verity.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != verity.ModeAuto {
		return s.mode, nil
	}
	err := s.CheckVerity()
	switch {
	case err == nil:
		s.mode = verity.ModeRequired
	case VerityUnsupported(err):
		slog.Debug("objectstore: the store's filesystem cannot enable fs-verity; storing objects without it", "root", s.root, "err", err)
		s.mode = verity.ModeOff
	default:
		return "", err
	}
	return s.mode, nil
}

// Preflight verifies this build can write to the store: in a build made in
// required mode the filesystem can enable fs-verity, so a pull fails before
// writing anything rather than partway through. An auto build's only check is
// the probe Mode makes once, and an off build has nothing to check.
func (s *Store) Preflight() error {
	if _, err := s.Mode(); err != nil {
		return err
	}
	if verity.Mode != verity.ModeRequired {
		return nil
	}
	if err := s.CheckVerity(); err != nil {
		return fmt.Errorf("%w; this build requires fs-verity, so point the store root at a filesystem with fs-verity support", err)
	}
	return nil
}

// requireVerity returns err from measuring an object in required mode, naming
// the mismatch when the object carries no fs-verity: required mode writes no
// such object, so the store holds objects another build wrote.
func requireVerity(err error) error {
	if noVerity(err) {
		return fmt.Errorf("object carries no fs-verity, which this build requires; the store holds objects written without it: remove the store and pull again, or enable fs-verity on its objects in place as docs/on-disk-layout.md describes: %w", err)
	}
	return err
}

// openError returns err from opening an object, naming the likely cause of
// EOPNOTSUPP: a kernel without fs-verity support refuses to open a file that
// has fs-verity.
func openError(err error) error {
	if errors.Is(err, unix.EOPNOTSUPP) {
		return fmt.Errorf("%w (a kernel without fs-verity support cannot open an object that has fs-verity)", err)
	}
	return err
}

// noVerity reports whether err from measuring an open object means the object
// carries no fs-verity.
func noVerity(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP)
}

// VerityUnsupported reports whether err from the fs-verity probe means the
// store's filesystem or kernel cannot enable fs-verity, as opposed to a
// failure to run the probe.
func VerityUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL)
}
