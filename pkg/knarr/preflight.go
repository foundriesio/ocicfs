// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"github.com/foundriesio/knarr/internal/composefs"
)

func (e *engine) PreflightTools() error {
	return composefs.Preflight()
}

func (e *engine) PreflightStore() error {
	return e.obj.CheckVerity()
}
