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
// its marker, and a mount can begin from an orphan binding a census already
// judged unwanted — under the shared lock none of those interleaves with a
// sweep. gc holds it exclusive and non-blocking: Linux flock has no writer
// fairness, so a blocking exclusive waiter could starve behind a stream of
// shared holders, and failing fast lets the driving agent retry. remove and
// release take no store lock — racing them only makes a collection keep too
// much for one cycle, never delete too much.
//
// The lock file is <root>/.lock, created on demand and never removed, the
// same pattern as the per-digest hold locks.

// lockStoreShared takes the store lock shared, blocking until it is free, and
// returns the release. The wait is not interruptible; a caller that must not
// proceed on a canceled context checks ctx.Err after acquiring, before its
// first mutation.
func (e *engine) lockStoreShared() (func(), error) {
	return e.lockStore(unix.LOCK_SH)
}

// lockStoreExclusive takes the store lock exclusive without blocking and
// returns the release. Held shared locks fail it with an error matching
// ErrStoreBusy.
func (e *engine) lockStoreExclusive() (func(), error) {
	return e.lockStore(unix.LOCK_EX | unix.LOCK_NB)
}

func (e *engine) lockStore(how int) (func(), error) {
	path := filepath.Join(e.root, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- store-root lock path
	if err != nil {
		return nil, fmt.Errorf("open store lock: %w", err)
	}
	err = unix.Flock(int(f.Fd()), how) // #nosec G115 -- a file descriptor always fits in int
	// flock is never auto-restarted (signal(7)), and the shared acquisition
	// can wait out a whole collection; a stray signal must not fail the verb.
	for errors.Is(err, unix.EINTR) {
		err = unix.Flock(int(f.Fd()), how) // #nosec G115 -- a file descriptor always fits in int
	}
	if err != nil {
		if cerr := f.Close(); cerr != nil {
			slog.Warn("knarr: failed to close store lock", "path", path, "err", cerr)
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("a pull, hold, or mount is in progress; retry: %w", ErrStoreBusy)
		}
		return nil, fmt.Errorf("lock store: %w", err)
	}
	return func() {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil { // #nosec G115 -- a file descriptor always fits in int
			slog.Warn("knarr: failed to unlock store lock", "path", path, "err", err)
		}
		if err := f.Close(); err != nil {
			slog.Warn("knarr: failed to close store lock", "path", path, "err", err)
		}
	}, nil
}
