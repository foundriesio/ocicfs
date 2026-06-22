// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package cli holds the command-line presentation layer shared by the knarr
// subcommands: output formatting (text and JSON), exit-code mapping, and
// argument validation. Result data is written to stdout; diagnostics and logs
// go to stderr, so a caller driving knarr can capture machine-readable output
// regardless of log verbosity.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/pkg/knarr"
)

// Format selects how a command renders its result. It satisfies pflag.Value so
// it can back a --format flag.
type Format string

const (
	// FormatText renders a compact, tab-separated line per record.
	FormatText Format = "text"
	// FormatJSON renders the result as indented JSON for programmatic consumers.
	FormatJSON Format = "json"
)

func (f *Format) String() string { return string(*f) }

// Set implements pflag.Value, rejecting anything but the known formats.
func (f *Format) Set(s string) error {
	switch Format(s) {
	case FormatText, FormatJSON:
		*f = Format(s)
		return nil
	default:
		return fmt.Errorf("invalid format %q (want %q or %q)", s, FormatText, FormatJSON)
	}
}

// Type implements pflag.Value; it names the flag's value in help text.
func (f *Format) Type() string { return "text|json" }

// AddFormatFlag registers a --format flag on cmd bound to f and wires its shell
// completion to the known formats.
func AddFormatFlag(cmd *cobra.Command, f *Format) {
	cmd.Flags().Var(f, "format", "output format")
	_ = cmd.RegisterFlagCompletionFunc("format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{string(FormatText), string(FormatJSON)}, cobra.ShellCompDirectiveNoFileComp
	})
}

// ImageOutput is the JSON/text shape for an image in command output. Its JSON
// field names are a stable contract for callers that parse knarr's output.
type ImageOutput struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func newImageOutput(img knarr.Image) ImageOutput {
	return ImageOutput{Ref: img.Ref, Digest: img.Digest, Size: img.Size}
}

// WriteImage renders a single image to w in the requested format.
func WriteImage(w io.Writer, format Format, img *knarr.Image) error {
	out := newImageOutput(*img)
	if format == FormatJSON {
		return emitJSON(w, out)
	}
	_, err := fmt.Fprintf(w, "%s\t%s\n", out.Ref, out.Digest)
	return err
}

// WriteImages renders a list of images to w in the requested format. JSON
// always yields an array, including the empty case ("[]").
func WriteImages(w io.Writer, format Format, imgs []knarr.Image) error {
	out := make([]ImageOutput, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, newImageOutput(img))
	}
	if format == FormatJSON {
		return emitJSON(w, out)
	}
	for _, o := range out {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%d\n", o.Ref, o.Digest, o.Size); err != nil {
			return err
		}
	}
	return nil
}

// emitJSON writes v to w as indented JSON followed by a newline.
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// UsageError marks an error caused by incorrect command-line usage (a bad flag
// or the wrong number of arguments) rather than a runtime failure. It maps to
// exit code 2 and causes the command's usage to be printed.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

// ExactArgs returns a positional-argument validator that reports a UsageError
// when the count is not n.
func ExactArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return &UsageError{Err: fmt.Errorf("expected %d argument(s), got %d", n, len(args))}
		}
		return nil
	}
}

// FlagErrorFunc wraps a flag-parsing error as a UsageError so it maps to exit
// code 2. Install it on the root command with SetFlagErrorFunc.
func FlagErrorFunc(_ *cobra.Command, err error) error {
	return &UsageError{Err: err}
}

// HandleExit converts the error returned by cobra's ExecuteC into a process
// exit code, printing diagnostics to errOut. cmd is the command cobra resolved,
// used to print usage on a usage error.
func HandleExit(cmd *cobra.Command, err error, errOut io.Writer) int {
	// Diagnostic writes to stderr are best-effort: a failed terminal write is
	// not actionable, so the error is discarded.
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		_, _ = fmt.Fprintln(errOut, "knarr: cancelled")
		return 130
	case isUsageError(err):
		_, _ = fmt.Fprintf(errOut, "knarr: %s\n", err.Error())
		if cmd != nil {
			_, _ = fmt.Fprint(errOut, cmd.UsageString())
		}
		return 2
	default:
		_, _ = fmt.Fprintf(errOut, "knarr: %s\n", err.Error())
		return 1
	}
}

// isUsageError reports whether err is a command-line usage error: either an
// explicit UsageError or one of cobra's command-resolution errors.
func isUsageError(err error) bool {
	var ue *UsageError
	if errors.As(err, &ue) {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command") ||
		strings.HasPrefix(msg, "unknown flag") ||
		strings.HasPrefix(msg, "unknown shorthand")
}
