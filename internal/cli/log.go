// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"log/slog"
)

// NewLogger returns a slog.Logger that writes to w (stderr in normal use). The
// level is debug when verbose, warn when quiet, otherwise info; quiet takes
// precedence over verbose. Logging is kept on stderr so it never contaminates
// the result data a command writes to stdout.
func NewLogger(w io.Writer, verbose, quiet bool) *slog.Logger {
	level := slog.LevelInfo
	switch {
	case quiet:
		level = slog.LevelWarn
	case verbose:
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
