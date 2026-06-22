// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import "context"

// engine is the default Engine. Its operations are not yet implemented and
// return ErrNotImplemented.
type engine struct{}

func (e *engine) Publish(ctx context.Context, dir, dest string) error { return ErrNotImplemented }

func (e *engine) Pull(ctx context.Context, ref string, opts ...PullOption) (*Image, error) {
	return nil, ErrNotImplemented
}

func (e *engine) Mount(ctx context.Context, ref, target string, opts ...MountOption) error {
	return ErrNotImplemented
}

func (e *engine) Unmount(ctx context.Context, target string) error { return ErrNotImplemented }

func (e *engine) List(ctx context.Context) ([]Image, error) { return nil, ErrNotImplemented }

func (e *engine) Remove(ctx context.Context, ref string, opts ...RemoveOption) error {
	return ErrNotImplemented
}
