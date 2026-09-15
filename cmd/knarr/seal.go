// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/internal/ociremote"
)

func newSealCommand(*globalOptions) *cobra.Command {
	var (
		platform          string
		tag               string
		dryRun            bool
		allowNonCanonical bool
		fromDir           string
		format            = cli.FormatText
	)

	cmd := &cobra.Command{
		Use:   "seal <name>@sha256:<digest>",
		Short: "add composefs integrity annotations to an image",
		Args:  cli.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := ociremote.Seal(cmd.Context(), args[0], ociremote.Options{
				Platform:          platform,
				Tag:               tag,
				DryRun:            dryRun,
				AllowNonCanonical: allowNonCanonical,
				FromDir:           fromDir,
			})
			if err != nil {
				return err
			}
			return cli.WriteSealResult(cmd.OutOrStdout(), format, result)
		},
	}

	cmd.Flags().StringVar(&platform, "platform", "", "index child to seal, as os/arch[/variant]; required for an index reference")
	cmd.Flags().StringVar(&tag, "tag", "", "also tag the sealed root in the input repository")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "compute the sealed digest without pushing")
	cmd.Flags().BoolVar(&allowNonCanonical, "allow-non-canonical", false, "seal a non-canonical construction under the knarr fallback annotations instead of declining")
	cmd.Flags().StringVar(&fromDir, "from-dir", "", "read the image from a local OCI image layout directory instead of the registry; pushes still go to the reference's repository")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
