// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import "errors"

// ErrNotImplemented is returned by engine operations that are not yet
// implemented.
var ErrNotImplemented = errors.New("not implemented")

// ErrNoRegistry reports that a pull requiring registry resolution has no
// RegistrySource configured on the engine.
var ErrNoRegistry = errors.New("no registry source configured")

// ErrNotFound reports that an image is not present in the local store for the
// requested platform.
var ErrNotFound = errors.New("image not found in local store")

// ErrNotMounted reports that an unmount target had no mount to tear down.
var ErrNotMounted = errors.New("target is not mounted")

// ErrAlreadyMounted reports that a mount target already carries a mount;
// mounts never stack, so the existing one must be unmounted first.
var ErrAlreadyMounted = errors.New("target is already mounted")

// ErrStoreBusy reports that gc could not take the exclusive store lock
// because a pull, hold, or mount is in progress; the caller retries.
var ErrStoreBusy = errors.New("store busy")
