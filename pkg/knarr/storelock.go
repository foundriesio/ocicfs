// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The store-level lock coordinates the verbs that make content wanted or in
// use with the collection that deletes what is neither. pull, hold, and mount
// hold it shared for their whole operation: a pull writes objects before the
// ref record that makes them wanted, a hold checks presence before writing
// its marker, and a mount can begin from an orphan binding during the
// inventory, before anything is judged — under the shared lock none of
// those interleaves with a sweep. gc holds it shared while it inventories
// the objects and exclusive while it decides and deletes, always
// non-blocking: Linux flock has no writer fairness, so a blocking exclusive
// waiter could starve behind a stream of shared holders, and failing fast
// lets the driving agent retry. remove and release take no store lock —
// racing them only makes a collection keep too much for one cycle, never
// delete too much.
//
// The lock file is <root>/.lock, created on demand and never removed, the
// same pattern as the per-digest hold locks.

// lockStoreShared takes the store lock shared, blocking until it is free, and
// returns the release. The wait is not interruptible; a caller that must not
// proceed on a canceled context checks ctx.Err after acquiring, before its
// first mutation.
func (e *engine) lockStoreShared() (func(), error) {
	f, err := e.openStoreLock(unix.LOCK_SH)
	if err != nil {
		return nil, err
	}
	return releaseStoreLock(f), nil
}

// lockStoreExclusive takes the store lock exclusive without blocking and
// returns the release. Held shared locks fail it with an error matching
// ErrStoreBusy.
func (e *engine) lockStoreExclusive() (func(), error) {
	f, err := e.openStoreLock(unix.LOCK_EX | unix.LOCK_NB)
	if err != nil {
		return nil, err
	}
	return releaseStoreLock(f), nil
}

// openStoreLock opens the lock file and takes it as how says. The caller
// may convert the lock with flockStore and releases it with
// releaseStoreLock.
func (e *engine) openStoreLock(how int) (*os.File, error) {
	path := filepath.Join(e.root, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- store-root lock path
	if err != nil {
		return nil, fmt.Errorf("open store lock: %w", err)
	}
	if err := flockStore(f, how); err != nil {
		if cerr := f.Close(); cerr != nil {
			slog.Warn("knarr: failed to close store lock", "path", path, "err", cerr)
		}
		return nil, err
	}
	return f, nil
}

// flockStore takes the store lock on f as how says, or converts the lock f
// already holds. A conversion drops the held lock before taking the new
// one, so a non-blocking conversion that fails leaves f holding no lock.
// A would-block failure is reported as an error matching ErrStoreBusy.
func flockStore(f *os.File, how int) error {
	err := unix.Flock(int(f.Fd()), how) // #nosec G115 -- a file descriptor always fits in int
	// flock is never auto-restarted (signal(7)), and the shared acquisition
	// can wait out a whole collection; a stray signal must not fail the verb.
	for errors.Is(err, unix.EINTR) {
		err = unix.Flock(int(f.Fd()), how) // #nosec G115 -- a file descriptor always fits in int
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return fmt.Errorf("another store operation is in progress; retry: %w", ErrStoreBusy)
	}
	if err != nil {
		return fmt.Errorf("lock store: %w", err)
	}
	return nil
}

// releaseStoreLock returns the function that drops whatever lock f holds
// and closes it.
func releaseStoreLock(f *os.File) func() {
	return func() {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil { // #nosec G115 -- a file descriptor always fits in int
			slog.Warn("knarr: failed to unlock store lock", "path", f.Name(), "err", err)
		}
		if err := f.Close(); err != nil {
			slog.Warn("knarr: failed to close store lock", "path", f.Name(), "err", err)
		}
	}
}
