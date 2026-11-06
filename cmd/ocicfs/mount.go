// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func newMountCommand(g *globalOptions) *cobra.Command {
	var rw bool
	var metadataOnly bool

	cmd := &cobra.Command{
		Use:   "mount [--metadata-only] <ref> <path>",
		Short: "mount an image read-only at a target path",
		Args:  cli.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rw && metadataOnly {
				return &cli.UsageError{Err: errors.New("--rw and --metadata-only are mutually exclusive")}
			}
			eng, err := g.engine()
			if err != nil {
				return err
			}
			var opts []ocicfs.MountOption
			if rw {
				opts = append(opts, ocicfs.WithWritable())
			}
			if metadataOnly {
				opts = append(opts, ocicfs.WithMetadataOnly())
			}
			return eng.Mount(cmd.Context(), args[0], args[1], opts...)
		},
	}

	cmd.Flags().BoolVar(&rw, "rw", false, "unsupported; use --metadata-only to build a writable overlay")
	cmd.Flags().BoolVar(&metadataOnly, "metadata-only", false, "mount only the image's verified EROFS metadata, without file data; the caller assembles its own overlay with the store's objects/fsverity-sha256-12 view as a data-only lowerdir, adding verity=require unless ocicfs version reports verity: disabled")
	return cmd
}
