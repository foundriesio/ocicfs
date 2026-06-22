// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
)

func newPublishCommand(g *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "publish <dir> <dest>",
		Short: "build a directory into a zstd:chunked OCI image and push it",
		Args:  cli.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			return eng.Publish(cmd.Context(), args[0], args[1])
		},
	}
}
