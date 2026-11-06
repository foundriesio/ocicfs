// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/ociremote"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
	"github.com/foundriesio/ocicfs/pkg/version"
)

const defaultRoot = "/var/lib/ocicfs"

// globalOptions holds the settings shared by every subcommand. The root
// command's persistent flags bind into it, and PersistentPreRunE fills in the
// output writers and logger before any subcommand runs.
type globalOptions struct {
	root        string
	overlayRoot string
	verbose     bool
	quiet       bool

	stdout io.Writer
	stderr io.Writer
	logger *slog.Logger
}

// config returns the engine configuration named by the resolved global
// options; a subcommand with settings of its own adds them before
// constructing the engine.
func (g *globalOptions) config() ocicfs.Config {
	return ocicfs.Config{
		Root:        g.root,
		OverlayRoot: g.overlayRoot,
		RegistrySource: func(ctx context.Context, ref string) (ocicfs.BlobSource, digest.Digest, error) {
			return ociremote.Open(ctx, ref)
		},
	}
}

// engine constructs the ocicfs engine from the resolved global options.
func (g *globalOptions) engine() (ocicfs.Engine, error) {
	return ocicfs.New(g.config())
}

// newRootCommand builds the ocicfs command tree. stdout and stderr are the
// initial output streams; they are also recorded on the returned options so
// tests can drive the CLI with buffers.
func newRootCommand(stdout, stderr io.Writer) (*cobra.Command, *globalOptions) {
	g := &globalOptions{stdout: stdout, stderr: stderr}

	root := &cobra.Command{
		Use:   "ocicfs",
		Short: "OCI-native content delivery and mount engine for IoT/edge devices",
		Long: "ocicfs pulls, verifies, and mounts OCI content as composefs images. It " +
			"provides primitives — seal, copy, pull, mount, unmount, inspect, ls, hold, release, remove, gc — " +
			"and leaves version management and update orchestration to the upper-layer " +
			"agent that drives it.",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			g.stdout = cmd.OutOrStdout()
			g.stderr = cmd.ErrOrStderr()
			g.logger = cli.NewLogger(g.stderr, g.verbose, g.quiet)
			slog.SetDefault(g.logger)
			return nil
		},
	}

	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetVersionTemplate("ocicfs {{.Version}}\n")
	root.SetFlagErrorFunc(cli.FlagErrorFunc)

	pf := root.PersistentFlags()
	pf.StringVar(&g.root, "root", defaultRoot, "ocicfs data directory")
	pf.StringVar(&g.overlayRoot, "overlay-root", "", "unused: writable mounts are not supported")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "enable debug logging on stderr")
	pf.BoolVarP(&g.quiet, "quiet", "q", false, "suppress non-error logging")

	root.AddCommand(
		newPublishCommand(g),
		newSealCommand(g),
		newCopyCommand(g),
		newPullCommand(g),
		newMountCommand(g),
		newUnmountCommand(g),
		newLsCommand(g),
		newInspectCommand(g),
		newHoldCommand(g),
		newReleaseCommand(g),
		newRemoveCommand(g),
		newGCCommand(g),
		newVersionCommand(g),
	)
	return root, g
}
