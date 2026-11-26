// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
)

func newFsckCommand(g *globalOptions) *cobra.Command {
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "fsck [REF...]",
		Short: "check stored content against its digests",
		Long: `Check that the store still holds what was pulled, reading only local state.

With references, fsck checks the images they name: the metadata, the
composefs image, and every file object the image needs. With none, it checks
every object in the store and every image in it, kept by a reference or a
hold or not.

Each object is read once and its bytes compared with both of its names. An
object is reported as
  damaged   when its bytes match none of its names or cannot be read back,
  misnamed  when its bytes are intact but a name is absent or wrong,
  missing   when an image needs it and the store does not have it.

fsck reports what it could read back; it is not a scan of the storage medium.

Exit status: 0 when everything checked is intact, 3 when damage was found,
1 when the check could not run or could not finish.`,
		Example: `  ocicfs fsck
  ocicfs fsck app@sha256:...
  ocicfs fsck --format json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := g.engine()
			if err != nil {
				return err
			}
			report, err := eng.Fsck(cmd.Context(), args)
			if report != nil {
				if werr := cli.WriteFsckReport(cmd.OutOrStdout(), format, report); werr != nil {
					return werr
				}
			}
			if err != nil {
				return err
			}
			if !report.Intact {
				return cli.ErrDamageFound
			}
			return nil
		},
	}

	cli.AddFormatFlag(cmd, &format)
	return cmd
}
