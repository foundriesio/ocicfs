// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func newFsckCommand(g *globalOptions) *cobra.Command {
	var fix, force bool
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "fsck [REF...] | fsck --fix [--force]",
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

With --fix, which takes no reference, fsck then repairs the whole store: it
restores the names of misnamed objects, deletes damaged objects, and removes
the binding of every image left without an object it needs. Such an image is
then "wanted, but not pulled": it cannot be mounted or inspected, and gc
refuses to run, until the image is pulled again from its source. Run --fix
when that source is reachable, and pull every image the report marks. A
damaged object that a mounted image uses is left unless --force is given;
with it the file disappears under the mount until the pull restores it.

Exit status: 0 when everything checked is intact, 3 when damage was found,
1 when the check could not run or could not finish.`,
		Example: `  ocicfs fsck
  ocicfs fsck app@sha256:...
  ocicfs fsck --format json
  ocicfs fsck --fix
  ocicfs pull app@sha256:...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts []ocicfs.FsckOption
			if fix {
				if len(args) > 0 {
					pulls := make([]string, len(args))
					for i, ref := range args {
						pulls[i] = "`ocicfs pull " + ref + "`"
					}
					return &cli.UsageError{Err: fmt.Errorf("--fix repairs the whole store and takes no reference, because a damaged object is usually shared between images; run `ocicfs fsck --fix`, then %s", strings.Join(pulls, ", "))}
				}
				opts = append(opts, ocicfs.WithFix())
			}
			if force {
				if !fix {
					return &cli.UsageError{Err: errors.New("--force requires --fix")}
				}
				opts = append(opts, ocicfs.WithFixForce())
			}
			eng, err := g.engine()
			if err != nil {
				return err
			}
			report, err := eng.Fsck(cmd.Context(), args, opts...)
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

	cmd.Flags().BoolVar(&fix, "fix", false, "repair the whole store; affected images must then be pulled again")
	cmd.Flags().BoolVar(&force, "force", false, "with --fix, also delete damaged objects a mounted image uses")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}
