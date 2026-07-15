// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/pkg/knarr"
)

func newPullCommand(g *globalOptions) *cobra.Command {
	var fromDir string
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "pull [--from-dir <dir>] <ref>",
		Short: "fetch an image into the local store",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			var opts []knarr.PullOption
			if fromDir != "" {
				opts = append(opts, knarr.WithFromDir(fromDir))
			}
			img, err := eng.Pull(cmd.Context(), args[0], opts...)
			if err != nil {
				return err
			}
			return cli.WriteImage(cmd.OutOrStdout(), format, img)
		},
	}

	cmd.Flags().StringVar(&fromDir, "from-dir", "", "sideload from an OCI image layout directory instead of a registry")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
