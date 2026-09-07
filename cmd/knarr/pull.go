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
	var ingestWorkers int
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "pull [--from-dir <dir>] [--ingest-workers <n>] <ref>",
		Short: "fetch an image into the local store",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := g.config()
			cfg.IngestWorkers = ingestWorkers
			eng, err := knarr.New(cfg)
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
	cmd.Flags().IntVar(&ingestWorkers, "ingest-workers", knarr.DefaultIngestWorkers, "layers scanned (and, from a registry, downloaded) concurrently; more than the default helps only on fast storage")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
