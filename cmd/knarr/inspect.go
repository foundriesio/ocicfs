// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
)

func newInspectCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "inspect REF",
		Short: "show a local image's identity and composefs binding",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			info, err := eng.Inspect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return cli.WriteImageInfo(cmd.OutOrStdout(), format, info)
		},
	}

	cli.AddFormatFlag(cmd, &format)
	return cmd
}
