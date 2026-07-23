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
	"text/tabwriter"
	"unicode"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/ociseal"
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

// ImageInfoOutput is the JSON/text shape for `knarr inspect` output. Its JSON
// field names are a stable contract for callers that parse knarr's output.
// RootfsSize is the merged rootfs's apparent size in bytes (regular-file
// content, hardlink groups counted once), omitted when the local binding
// predates the field.
type ImageInfoOutput struct {
	Root       string           `json:"root"`
	Platform   string           `json:"platform"`
	DiffIDs    []string         `json:"diff_ids"`
	Composefs  DescriptorOutput `json:"composefs"`
	Fsverity   string           `json:"fsverity"`
	RootfsSize int64            `json:"rootfs_size,omitempty"`
	Layers     []LayerOutput    `json:"layers,omitempty"`
}

// DescriptorOutput is the JSON shape for an OCI descriptor in command output.
type DescriptorOutput struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// LayerOutput is the JSON shape for one layer in `inspect --layers` output.
// Size is the layer blob's byte size from the manifest descriptor — the
// archive as transferred, compressed for +gzip/+zstd media types.
type LayerOutput struct {
	Index     int    `json:"index"`
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	DiffID    string `json:"diffID"`
	ChainID   string `json:"chainID"`
	Size      int64  `json:"size"`
	CreatedBy string `json:"createdBy,omitempty"`
}

// SealResultOutput is the JSON shape for `knarr seal` output. Every digest is
// paired with the reference that names it in the sealed repository, so sealing
// the next platform of an index consumes the previous call's output verbatim
// instead of rebuilding the reference from its parts.
type SealResultOutput struct {
	Original    string               `json:"original"`
	OriginalRef string               `json:"original_ref"`
	Sealed      string               `json:"sealed"`
	SealedRef   string               `json:"sealed_ref"`
	Platforms   []SealPlatformOutput `json:"platforms"`
}

// SealPlatformOutput is one platform manifest in `knarr seal` output. For an
// index reference the top-level digests are the index's, so the manifest this
// platform had before sealing is reported here; equal original and sealed
// manifests mean the platform was already sealed.
type SealPlatformOutput struct {
	Platform            string `json:"platform"`
	OriginalManifest    string `json:"original_manifest"`
	OriginalManifestRef string `json:"original_manifest_ref"`
	SealedManifest      string `json:"sealed_manifest"`
	SealedManifestRef   string `json:"sealed_manifest_ref"`
	Fsverity            string `json:"fsverity"`
}

// WriteSealResult renders a seal result to w in the requested format.
func WriteSealResult(w io.Writer, format Format, result ociseal.Result) error {
	out := SealResultOutput{
		Original:    result.Original.String(),
		OriginalRef: digestRef(result.Repository, result.Original),
		Sealed:      result.Sealed.String(),
		SealedRef:   digestRef(result.Repository, result.Sealed),
		Platforms:   make([]SealPlatformOutput, 0, len(result.Platforms)),
	}
	for _, platform := range result.Platforms {
		out.Platforms = append(out.Platforms, SealPlatformOutput{
			Platform:            ocilayout.PlatformString(&platform.Platform),
			OriginalManifest:    platform.Original.String(),
			OriginalManifestRef: digestRef(result.Repository, platform.Original),
			SealedManifest:      platform.Sealed.String(),
			SealedManifestRef:   digestRef(result.Repository, platform.Sealed),
			Fsverity:            platform.MergedFsverity,
		})
	}
	if format == FormatJSON {
		return emitJSON(w, out)
	}
	if _, err := fmt.Fprintf(w, "original  %s\nsealed    %s\n", out.OriginalRef, out.SealedRef); err != nil {
		return err
	}
	// A seal call handles exactly one platform, so this is one row; the sealed
	// manifest digest stays JSON-only (for an index it is recoverable from the
	// sealed index, and the text output reads as label/value rows).
	for _, platform := range out.Platforms {
		if _, err := fmt.Fprintf(w, "fsverity  %s\n", platform.Fsverity); err != nil {
			return err
		}
	}
	return nil
}

// digestRef names d in repo, the form a registry client accepts. A result
// carrying no repository renders the bare digest.
func digestRef(repo string, d digest.Digest) string {
	if repo == "" {
		return d.String()
	}
	return repo + "@" + d.String()
}

// WriteImageInfo renders an inspected image to w in the requested format.
// withLayers appends the per-layer view — an aligned table in text output,
// the layers array in JSON; without it the output is unchanged from before
// the layer view existed.
func WriteImageInfo(w io.Writer, format Format, info *knarr.ImageInfo, withLayers bool) error {
	diffIDs := make([]string, 0, len(info.DiffIDs))
	for _, d := range info.DiffIDs {
		diffIDs = append(diffIDs, d.String())
	}
	out := ImageInfoOutput{
		Root:     info.Root.String(),
		Platform: ocilayout.PlatformString(&info.Platform),
		DiffIDs:  diffIDs,
		Composefs: DescriptorOutput{
			MediaType: info.Composefs.MediaType,
			Digest:    info.Composefs.Digest.String(),
			Size:      info.Composefs.Size,
		},
		Fsverity:   info.FsverityDigest,
		RootfsSize: info.RootfsSize,
	}
	if withLayers {
		out.Layers = make([]LayerOutput, 0, len(info.Layers))
		for i, l := range info.Layers {
			out.Layers = append(out.Layers, LayerOutput{
				Index:     i,
				MediaType: l.MediaType,
				Digest:    l.Digest.String(),
				DiffID:    l.DiffID.String(),
				ChainID:   l.ChainID.String(),
				Size:      l.Size,
				CreatedBy: l.CreatedBy,
			})
		}
	}
	if format == FormatJSON {
		return emitJSON(w, out)
	}

	// Text output is the image's identity and binding; per-layer detail
	// (including the diff_ids) renders only as the --layers table. JSON always
	// carries diff_ids — that list is part of the stable contract. The rows go
	// through a tabwriter so the value column is aligned for human eyes;
	// machine consumers parse the JSON format, not this.
	rows := [][2]string{
		{"root", out.Root},
		{"platform", out.Platform},
		{"composefs", out.Composefs.Digest},
		{"fsverity", out.Fsverity},
	}
	// The merged rootfs's apparent size is recorded at build time; a binding
	// written before the field existed reports 0, and the row is left out
	// rather than shown as a bogus zero.
	if out.RootfsSize > 0 {
		rows = append(rows, [2]string{"rootfs", humanBytes(out.RootfsSize)})
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if withLayers {
		return writeLayerTable(w, out.Layers)
	}
	return nil
}

// writeLayerTable renders the per-layer view as an aligned table. Digests
// are abbreviated to keep rows terminal-sized; the JSON format carries the
// full digest values plus the createdBy history command, which the table
// omits.
func writeLayerTable(w io.Writer, layers []LayerOutput) error {
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "#\tBLOB DIGEST\tDIFFID\tCHAINID\tARCHIVE\tMEDIA TYPE"); err != nil {
		return err
	}
	for _, l := range layers {
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			l.Index,
			shortHex(l.Digest),
			shortHex(l.DiffID),
			shortHex(l.ChainID),
			humanBytes(l.Size),
			scrubControl(l.MediaType),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// scrubControl replaces control characters — C0, DEL, and the C1 range,
// whose 8-bit escape forms terminals may also interpret — with spaces so
// publisher-authored strings (media types) cannot inject escape sequences or
// forged rows into the terminal. The JSON path needs no scrubbing:
// encoding/json escapes control characters.
func scrubControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// shortHex abbreviates a sha256 digest string to its first 12 hex characters
// for table display; an unexpectedly shaped value passes through scrubbed.
func shortHex(s string) string {
	hex, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(hex) < 12 {
		return scrubControl(s)
	}
	return hex[:12]
}

// humanBytes renders a byte count as B/KiB/MiB/… with one decimal.
func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v, unit := float64(n)/1024, 0
	for v >= 1024 {
		v, unit = v/1024, unit+1
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTPE"[unit])
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
