// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/pkg/knarr"
)

func newMountCommand(g *globalOptions) *cobra.Command {
	var rw bool
	var metadataOnly bool

	cmd := &cobra.Command{
		Use:   "mount [--rw|--metadata-only] <ref> <path>",
		Short: "mount an image at a target path",
		Args:  cli.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rw && metadataOnly {
				return &cli.UsageError{Err: errors.New("--rw and --metadata-only are mutually exclusive")}
			}
			eng, err := g.engine()
			if err != nil {
				return err
			}
			var opts []knarr.MountOption
			if rw {
				opts = append(opts, knarr.WithWritable())
			}
			if metadataOnly {
				opts = append(opts, knarr.WithMetadataOnly())
			}
			return eng.Mount(cmd.Context(), args[0], args[1], opts...)
		},
	}

	cmd.Flags().BoolVar(&rw, "rw", false, "mount a writable overlay over the read-only composefs base, with the upperdir and workdir allocated under the overlay root")
	cmd.Flags().BoolVar(&metadataOnly, "metadata-only", false, "mount only the image's verified EROFS metadata, without file data; the caller assembles its own overlay with the store's objects/sha256 directory as a data-only lowerdir and verity=require")
	return cmd
}
