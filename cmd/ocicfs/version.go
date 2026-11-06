// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/pkg/version"
)

func newVersionCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "version",
		Short: "show the version and the fs-verity state",
		Long: `Version prints the ocicfs version and whether this binary runs the store
under --root with fs-verity, with the fs-verity mode it was built with:

  ocicfs <version> (<commit>)
  verity: enabled (build: auto)

The state is enabled, disabled, unsupported, or unknown when it cannot be
determined, in which case the command fails with the cause; see "The
fs-verity mode" in docs/on-disk-layout.md.`,
		Args: cli.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The version and build mode are printed even when the store
			// cannot be opened or its state cannot be determined.
			var state string
			eng, err := g.engine()
			if err == nil {
				state, err = eng.VerityState()
			}
			if werr := cli.WriteVersion(cmd.OutOrStdout(), format, version.Version, version.Revision, state); werr != nil {
				return werr
			}
			return err
		},
	}
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
