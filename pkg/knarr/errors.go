// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import "errors"

// ErrNotImplemented is returned by engine operations that are not yet
// implemented.
var ErrNotImplemented = errors.New("not implemented")

// ErrNotFound reports that an image is not present in the local store for the
// requested platform.
var ErrNotFound = errors.New("image not found in local store")

// ErrNotMounted reports that an unmount target had no mount to tear down.
var ErrNotMounted = errors.New("target is not mounted")

// ErrStoreBusy reports that gc could not take the exclusive store lock
// because a pull, hold, or mount is in progress; the caller retries.
var ErrStoreBusy = errors.New("store busy")
