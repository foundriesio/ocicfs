// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Command knarr is the command-line front end for the knarr content delivery
// and mount engine. It dispatches a small set of primitive verbs (publish,
// seal, pull, mount, unmount, inspect, ls, remove) to the knarr engine. All verbs
// but publish are wired; publish returns "not implemented".
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/foundriesio/knarr/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point: it builds the command tree, executes it
// against argv, and maps the result to a process exit code. Result data is
// written to stdout and diagnostics to stderr.
func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	root, _ := newRootCommand(stdout, stderr)

	// A bare invocation with no subcommand is a usage error, not a request for
	// help: report it on stderr and exit 2 so scripts can detect it.
	if len(argv) == 0 {
		root.SetOut(stderr)
		_ = root.Usage()
		return 2
	}

	root.SetArgs(argv)
	root.SetContext(ctx)
	cmd, err := root.ExecuteC()
	return cli.HandleExit(cmd, err, stderr)
}
