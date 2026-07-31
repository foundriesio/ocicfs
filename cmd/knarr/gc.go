// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/pkg/knarr"
)

func newGCCommand(g *globalOptions) *cobra.Command {
	var dryRun, listRemoved, human bool
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "gc [--dry-run]",
		Short: "delete store content that is neither wanted nor in use",
		Long: `Delete every store object that is neither wanted - reachable from a
reference record or a hold - nor in use by a mounter, along with orphan
bindings, orphan fs-verity view entries, staging leftovers, and the empty
shard directories earlier deletions left behind. A wanted image gc cannot
fully enumerate aborts the collection with nothing deleted; while a pull,
hold, or mount is in progress gc reports the store as busy, and the caller
retries.`,
		Example: `  knarr gc --dry-run
  knarr gc --list-removed --human
  knarr gc --format json`,
		Args: cli.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			var opts []knarr.GCOption
			if dryRun {
				opts = append(opts, knarr.WithDryRun())
			}
			report, err := eng.GC(cmd.Context(), opts...)
			if err != nil {
				return err
			}
			return cli.WriteGCReport(cmd.OutOrStdout(), format, report, cli.GCOutputOptions{
				DryRun:      dryRun,
				ListRemoved: listRemoved,
				Human:       human,
			})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed without removing anything")
	cmd.Flags().BoolVar(&listRemoved, "list-removed", false, "list each removed object's digest")
	cmd.Flags().BoolVar(&human, "human", false, "print sizes in human-readable units (KiB, MiB, ...)")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
