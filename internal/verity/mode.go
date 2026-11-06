// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package verity

// Policy is an fs-verity policy: the mode a binary is built with, or the mode
// it runs a store in.
type Policy string

const (
	// ModeAuto runs a store with fs-verity when its filesystem can enable
	// it, and without otherwise. It is a build mode only; a store is run in
	// required or off mode.
	ModeAuto Policy = "auto"
	// ModeRequired gives every object fs-verity and requires it on every
	// object read.
	ModeRequired Policy = "required"
	// ModeOff gives no object fs-verity and requires it on none.
	ModeOff Policy = "off"
)
