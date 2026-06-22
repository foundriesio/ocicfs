// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
)

func newUnmountCommand(g *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "unmount <path>",
		Short: "tear down a mount",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			return eng.Unmount(cmd.Context(), args[0])
		},
	}
}
