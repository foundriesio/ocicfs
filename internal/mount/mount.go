// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package mount owns read-only composefs mount operations.
package mount

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// Spec describes one read-only composefs mount.
type Spec struct {
	ImagePath      string
	Basedir        string
	Target         string
	FsverityDigest string
}

// ErrNotMounted reports that the unmount target had no mount to tear down, so
// callers with idempotent teardown paths can match it with errors.Is.
var ErrNotMounted = errors.New("mount: target is not mounted")

// ReadOnly mounts s.ImagePath at s.Target with fs-verity required.
func ReadOnly(ctx context.Context, s Spec) error {
	opts, err := composefsOptions(s)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "mount.composefs", "-o", opts, "--", s.ImagePath, s.Target) // #nosec G204 -- fixed binary, discrete validated args, "--" ends option parsing
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return commandError(ctxErr, stderr.String())
		}
		return commandError(err, stderr.String())
	}
	return nil
}

// Unmount tears down the mount at target. A target with no mount returns an
// error matching ErrNotMounted.
func Unmount(target string) error {
	if target == "" {
		return errors.New("mount: empty target")
	}
	if err := unix.Unmount(target, 0); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("mount: %s: %w", target, errors.Join(ErrNotMounted, err))
		}
		return fmt.Errorf("mount: unmount %s: %w", target, err)
	}
	return nil
}

// composefsOptions validates s and builds the -o string that carries the trust
// posture: the image's expected fs-verity digest, fs-verity required on every
// redirected object, and a read-only mount.
func composefsOptions(s Spec) (string, error) {
	if err := validateSpec(s); err != nil {
		return "", err
	}
	digestHex, err := fsverityDigestHex(s.FsverityDigest)
	if err != nil {
		return "", err
	}
	basedir, err := optionValue("basedir", s.Basedir)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"basedir=" + basedir,
		"digest=" + digestHex,
		"verity",
		"ro",
	}, ","), nil
}

func validateSpec(s Spec) error {
	switch {
	case s.ImagePath == "":
		return errors.New("mount: empty image path")
	case s.Basedir == "":
		return errors.New("mount: empty basedir")
	case s.Target == "":
		return errors.New("mount: empty target")
	case s.FsverityDigest == "":
		return errors.New("mount: empty fs-verity digest")
	}
	return nil
}

func fsverityDigestHex(raw string) (string, error) {
	d, err := digest.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("mount: fs-verity digest: %w", err)
	}
	if d.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("mount: fs-verity digest: non-sha256 digest algorithm %s", d.Algorithm())
	}
	if err := d.Validate(); err != nil {
		return "", fmt.Errorf("mount: fs-verity digest: %w", err)
	}
	return d.Hex(), nil
}

// optionValue returns value for use in a mount.composefs -o option, rejecting
// the delimiter bytes that would split it: ',' separates options and ':'
// separates multiple basedirs.
func optionValue(name, value string) (string, error) {
	if strings.ContainsAny(value, ":,") {
		return "", fmt.Errorf("mount: %s cannot contain ':' or ','", name)
	}
	return value, nil
}

func commandError(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("mount: mount.composefs: %w: %s", err, stderr)
	}
	return fmt.Errorf("mount: mount.composefs: %w", err)
}
