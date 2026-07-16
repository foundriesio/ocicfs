// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
)

func newInspectCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText
	var layers bool

	cmd := &cobra.Command{
		Use:   "inspect [--layers] REF",
		Short: "show a local image's identity and composefs binding",
		Long: `Inspect reports the OCI identity of an image already in the local store and
the composefs image built for it, reading only local state.

With --layers it adds a per-layer view. The archive size is the layer blob's
size from the manifest descriptor (compressed for +gzip/+zstd layers); the
diff_id is the digest of the layer's uncompressed tar stream; the chain ID is
the cumulative identity derived from the ordered diff_ids. The JSON format
additionally carries each layer's history command (createdBy).

Examples:
  knarr inspect app@sha256:...
  knarr inspect --layers app@sha256:...
  knarr inspect --layers --format json app@sha256:...`,
		Args: cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			info, err := eng.Inspect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return cli.WriteImageInfo(cmd.OutOrStdout(), format, info, layers)
		},
	}

	cmd.Flags().BoolVar(&layers, "layers", false, "include a per-layer view: blob digest, diff_id, chain ID, archive size, media type (JSON adds the history command)")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
