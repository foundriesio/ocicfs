// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/verity"
)

// BuildVerityMode is the fs-verity mode this binary was built with: "auto"
// (the default), "required" (build tag ocicfs_verity_required) or "off"
// (build tag ocicfs_verity_off). It alone decides whether the objects a pull
// writes get fs-verity and whether a mount requires it: required and off as
// named, auto by whether the store's filesystem can enable fs-verity.
const BuildVerityMode = string(verity.Mode)

func (e *engine) PreflightTools() error {
	return composefs.Preflight()
}

func (e *engine) PreflightStore() error {
	return e.obj.Preflight()
}

func (e *engine) VerityState() (string, error) {
	required, err := e.VerityRequired()
	if err != nil {
		return "", err
	}
	if !required {
		return "disabled", nil
	}
	// A build made in required mode runs every store with fs-verity without
	// probing its filesystem; the preflight probes it, so the state says
	// where a pull would fail.
	switch err := e.obj.Preflight(); {
	case err == nil:
		return "enabled", nil
	case objectstore.VerityUnsupported(err):
		return "unsupported", nil
	default:
		return "", err
	}
}

func (e *engine) VerityRequired() (bool, error) {
	mode, err := e.obj.Mode()
	if err != nil {
		return false, err
	}
	return mode != verity.ModeOff, nil
}
