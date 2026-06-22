// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/pkg/knarr"
)

func newMountCommand(g *globalOptions) *cobra.Command {
	var rw bool

	cmd := &cobra.Command{
		Use:   "mount [--rw] <ref> <path>",
		Short: "mount an image at a target path",
		Args:  cli.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			var opts []knarr.MountOption
			if rw {
				opts = append(opts, knarr.WithWritable())
			}
			return eng.Mount(cmd.Context(), args[0], args[1], opts...)
		},
	}

	cmd.Flags().BoolVar(&rw, "rw", false, "mount a writable overlay over the read-only composefs base, with the upperdir and workdir allocated under the overlay root")
	return cmd
}
