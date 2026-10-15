// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func newPullCommand(g *globalOptions) *cobra.Command {
	var fromDir string
	var delta bool
	var ingestWorkers int
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "pull [--from-dir <dir>] [--delta] [--ingest-workers <n>] <ref>",
		Short: "fetch an image into the local store",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := g.config()
			cfg.IngestWorkers = ingestWorkers
			eng, err := ocicfs.New(cfg)
			if err != nil {
				return err
			}
			var opts []ocicfs.PullOption
			if fromDir != "" {
				opts = append(opts, ocicfs.WithFromDir(fromDir))
			}
			if delta {
				opts = append(opts, ocicfs.WithDelta())
			}
			img, err := eng.Pull(cmd.Context(), args[0], opts...)
			if err != nil {
				return err
			}
			return cli.WriteImage(cmd.OutOrStdout(), format, img)
		},
	}

	cmd.Flags().StringVar(&fromDir, "from-dir", "", "sideload from an OCI image layout directory instead of a registry")
	cmd.Flags().BoolVar(&delta, "delta", false, "fetch only the files the store lacks from zstd:chunked layers; hold the previous release first (see docs/pull.md)")
	cmd.Flags().IntVar(&ingestWorkers, "ingest-workers", ocicfs.DefaultIngestWorkers, "layers scanned (and, from a registry, downloaded) concurrently; more than the default helps only on fast storage")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
