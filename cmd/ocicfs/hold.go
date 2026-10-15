// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
)

func newHoldCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText
	holder := "cli"

	cmd := &cobra.Command{
		Use:   "hold [--holder NAME] REF",
		Short: "pin a local image so remove declines while held",
		Long: `Hold records a named holder for an image already in the local store so that
a plain remove of that image declines while the holder is present. Holds are
keyed by the image's identity digest -- the platform image manifest, which a
reference naming an index is descended to -- and reading only local state; run
ocicfs pull first. Recording an already-present holder is a no-op success.`,
		Args: cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			if err := eng.Hold(cmd.Context(), args[0], holder); err != nil {
				return err
			}
			return cli.WriteHold(cmd.OutOrStdout(), format, args[0], holder, true)
		},
	}

	cmd.Flags().StringVar(&holder, "holder", holder, "name of the holder to record")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}

func newReleaseCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText
	holder := "cli"

	cmd := &cobra.Command{
		Use:   "release [--holder NAME] REF",
		Short: "drop a hold on a local image",
		Long: `Release drops a named holder's hold on an image's identity digest, resolving
the reference the way hold does. Releasing a holder that is not present is a
no-op success, as is releasing a digest whose content is gone from the store.`,
		Args: cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			if err := eng.Release(cmd.Context(), args[0], holder); err != nil {
				return err
			}
			return cli.WriteHold(cmd.OutOrStdout(), format, args[0], holder, false)
		},
	}

	cmd.Flags().StringVar(&holder, "holder", holder, "name of the holder to release")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
