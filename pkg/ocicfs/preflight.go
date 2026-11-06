// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/verity"
)

// BuildVerityMode is the fs-verity mode this binary was built with: "auto"
// (the default) or "required" (build tag ocicfs_verity_required). It alone
// decides whether the objects a pull writes get fs-verity and whether a mount
// requires it: required as named, auto by whether the store's filesystem can
// enable fs-verity.
const BuildVerityMode = string(verity.Mode)

func (e *engine) PreflightTools() error {
	return composefs.Preflight()
}

func (e *engine) PreflightStore() error {
	return e.obj.CheckVerity()
}
