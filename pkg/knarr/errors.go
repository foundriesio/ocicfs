// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import "errors"

// ErrNotImplemented is returned by engine operations that are not yet
// implemented.
var ErrNotImplemented = errors.New("not implemented")
