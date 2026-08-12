// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"github.com/foundriesio/knarr/internal/composefs"
)

func (e *engine) Preflight() error {
	if err := composefs.Preflight(); err != nil {
		return err
	}
	return e.obj.CheckVerity()
}
