// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func newRemoveCommand(g *globalOptions) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "remove [--force] <ref>",
		Short: "forget a local image",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			var opts []ocicfs.RemoveOption
			if force {
				opts = append(opts, ocicfs.WithForce())
			}
			return eng.Remove(cmd.Context(), args[0], opts...)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove even while held, clearing any holds first")
	return cmd
}
