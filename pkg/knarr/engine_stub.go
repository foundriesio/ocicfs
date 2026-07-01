// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import "context"

func (e *engine) Publish(ctx context.Context, dir, dest string) error { return ErrNotImplemented }

func (e *engine) Mount(ctx context.Context, ref, target string, opts ...MountOption) error {
	return ErrNotImplemented
}

func (e *engine) Unmount(ctx context.Context, target string) error { return ErrNotImplemented }

func (e *engine) List(ctx context.Context) ([]Image, error) { return nil, ErrNotImplemented }

func (e *engine) Remove(ctx context.Context, ref string, opts ...RemoveOption) error {
	return ErrNotImplemented
}
