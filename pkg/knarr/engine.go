// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package knarr is the engine API for the knarr content delivery and mount
// tool. It exposes the primitive operations exposed to the command-line front
// end and to embedders. The engine owns a local content-addressed store; it
// holds no version-management, activation, or update-orchestration policy.
//
// The current surface covers the basic primitives — publish, pull, mount,
// unmount, list, remove. The remaining primitives (verify, hold, release,
// status, gc) are added as they are implemented.
package knarr

import (
	"context"
	"errors"
)

// Image describes an image present in the local store.
type Image struct {
	// Ref is the local reference the image is stored under.
	Ref string
	// Digest is the image manifest digest, e.g. "sha256:…".
	Digest string
	// Size is the on-disk size of the image's content, in bytes.
	Size int64
}

// PullOptions is the resolved configuration a Pull acts on. Callers pass the
// With* options to Pull, which apply to a zero PullOptions.
type PullOptions struct {
	// FromDir sources content from an OCI image layout directory instead of a
	// registry (air-gapped sideload). Empty means pull from a registry.
	FromDir string
}

// PullOption opts a Pull into a particular behaviour.
type PullOption func(*PullOptions)

// WithFromDir sources content from an OCI image layout directory instead of a
// registry (air-gapped sideload).
func WithFromDir(dir string) PullOption {
	return func(o *PullOptions) { o.FromDir = dir }
}

// MountOptions is the resolved configuration a Mount acts on. Callers do not
// usually build it directly: they pass the With* options to Mount, which apply
// to a zero MountOptions. The zero value yields a read-only composefs mount; a
// writable overlay over that base is requested when any of Writable,
// OverlayRoot, UpperDir, or WorkDir is set.
type MountOptions struct {
	// Writable stacks a writable overlay over the read-only composefs base.
	Writable bool
	// OverlayRoot overrides Config.OverlayRoot for this mount: the per-mount
	// upperdir and workdir are allocated under it. Empty falls back to the
	// engine-wide Config.OverlayRoot.
	OverlayRoot string
	// UpperDir and WorkDir name the overlay's writable layer explicitly,
	// bypassing allocation under any overlay root. Both must be set together.
	UpperDir string
	WorkDir  string
}

// MountOption opts a Mount into a particular behaviour. The zero set of options
// yields a read-only composefs mount; the With* options opt into a writable
// overlay stacked over that read-only composefs base.
type MountOption func(*MountOptions)

// WithWritable stacks a writable overlay over the read-only composefs base,
// with the upperdir and workdir allocated under the overlay root (the
// per-mount WithOverlayRoot if given, else Config.OverlayRoot).
func WithWritable() MountOption {
	return func(o *MountOptions) { o.Writable = true }
}

// WithOverlayRoot allocates this mount's writable upperdir and workdir under
// dir instead of the engine-wide Config.OverlayRoot. It implies a writable
// overlay. A caller that owns a per-mount directory (for example a snapshotter
// keying directories to its own identity) points knarr at that directory and
// lets knarr lay out the upperdir and workdir under it.
func WithOverlayRoot(dir string) MountOption {
	return func(o *MountOptions) {
		o.OverlayRoot = dir
		o.Writable = true
	}
}

// WithUpperDir names the overlay's writable upper directory explicitly,
// bypassing allocation under an overlay root. It implies a writable overlay and
// must be paired with WithWorkDir.
func WithUpperDir(dir string) MountOption {
	return func(o *MountOptions) {
		o.UpperDir = dir
		o.Writable = true
	}
}

// WithWorkDir names the overlay work directory explicitly; it pairs with
// WithUpperDir and must be on the same filesystem as the upper directory.
func WithWorkDir(dir string) MountOption {
	return func(o *MountOptions) {
		o.WorkDir = dir
		o.Writable = true
	}
}

// RemoveOptions is the resolved configuration a Remove acts on. Callers pass
// the With* options to Remove, which apply to a zero RemoveOptions.
type RemoveOptions struct {
	// Force removes the image even while it is held against garbage collection.
	Force bool
}

// RemoveOption opts a Remove into a particular behaviour.
type RemoveOption func(*RemoveOptions)

// WithForce removes the image even while it is held against garbage collection.
func WithForce() RemoveOption {
	return func(o *RemoveOptions) { o.Force = true }
}

// Engine performs the knarr primitives against a local content-addressed store.
type Engine interface {
	// Publish builds the tree at dir into a zstd:chunked OCI image and pushes
	// it to dest.
	Publish(ctx context.Context, dir, dest string) error
	// Pull fetches ref into the local store, transferring only content the
	// store does not already hold, and returns the stored image.
	Pull(ctx context.Context, ref string, opts ...PullOption) (*Image, error)
	// Mount mounts ref at target: a single read-only composefs mount by
	// default, or, when a With* option opts in, a writable overlay over that
	// composefs base. Mount operates on content already present in the store
	// and does not fetch over the network; pull ref first.
	Mount(ctx context.Context, ref, target string, opts ...MountOption) error
	// Unmount tears down the mount at target.
	Unmount(ctx context.Context, target string) error
	// List returns the images present in the local store.
	List(ctx context.Context) ([]Image, error)
	// Remove forgets a local image.
	Remove(ctx context.Context, ref string, opts ...RemoveOption) error
}

// Config configures a new Engine.
type Config struct {
	// Root is the knarr data directory holding the content-addressed store.
	Root string
	// OverlayRoot is the default base directory under which a writable mount's
	// per-mount upperdir and workdir are allocated. Empty defaults to a
	// directory under Root. A per-mount WithOverlayRoot overrides it.
	OverlayRoot string
}

// New returns an Engine backed by the store under cfg.Root.
func New(cfg Config) (Engine, error) {
	if cfg.Root == "" {
		return nil, errors.New("knarr: empty root directory")
	}
	return &engine{}, nil
}
