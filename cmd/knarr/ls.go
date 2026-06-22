// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
)

func newLsCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "list local images",
		Args:  cli.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			images, err := eng.List(cmd.Context())
			if err != nil {
				return err
			}
			return cli.WriteImages(cmd.OutOrStdout(), format, images)
		},
	}

	cli.AddFormatFlag(cmd, &format)
	return cmd
}
