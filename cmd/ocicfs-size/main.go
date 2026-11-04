// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Command ocicfs-size estimates how much disk a set of OCI images takes when
// containerd stores them with the overlayfs snapshotter, the EROFS
// snapshotter, ocicfs's file CAS, or the layer-centric EROFS prototype. It
// reads the images' layers but never unpacks them; every figure is
// modelled, not measured.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/composefs"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/ociremote"
	"github.com/foundriesio/ocicfs/pkg/version"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, ociremote.Open))
}

// The --inline-max range, and the note for a threshold above what
// mkcomposefs accepts.
const (
	minInlineMax    = composefs.InlineContentMax
	maxInlineMax    = 1 << 20
	inlineBeyondCap = "inline above 5000 B assumes a writer beyond composefs-c 1.0.8's limit; sized with EROFS flat-inline layout"
)

// options are the images command's flags.
type options struct {
	platform     string
	fs           string
	hostMetadata string
	inlineMax    int64
	cacheDir     string
	noCache      bool
	details      bool
	json         bool
	verbose      bool
}

// run is the testable entry point: it runs the command argv names against
// the references open resolves and returns the process exit code.
func run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, open opener) int {
	root := &cobra.Command{
		Use:           "ocicfs-size",
		Short:         "Estimate the disk usage of OCI images under containerd snapshotters and ocicfs",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetVersionTemplate("ocicfs-size {{.Version}}\n")
	root.SetFlagErrorFunc(cli.FlagErrorFunc)

	var o options
	images := &cobra.Command{
		Use:   "images REF|@FILE...",
		Short: "Estimate the storage a set of images takes once pulled",
		Long: "Estimate the storage a set of images takes once pulled, under the overlayfs and " +
			"EROFS snapshotters, the ocicfs file CAS and the layer-centric EROFS prototype. " +
			"@FILE reads one reference per line (@- reads stdin).",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return &cli.UsageError{Err: errors.New("expected at least one image reference")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.SetDefault(cli.NewLogger(stderr, o.verbose, false))
			return runImages(cmd.Context(), args, o, stdin, stdout, open)
		},
	}
	f := images.Flags()
	f.StringVar(&o.platform, "platform", "", "platform to size, os/arch[/variant] (default linux/"+runtime.GOARCH+")")
	f.StringVar(&o.fs, "fs", "ext4", "host filesystem profile (only ext4)")
	f.StringVar(&o.hostMetadata, "host-metadata", "include", "count host directories and small files: include or exclude")
	f.Int64Var(&o.inlineMax, "inline-max", composefs.InlineContentMax, fmt.Sprintf("largest regular file, in bytes, File CAS/image stores inline instead of as an object (%d-%d)", minInlineMax, maxInlineMax))
	f.StringVar(&o.cacheDir, "cache-dir", "", "layer inventory cache (default $XDG_CACHE_HOME/ocicfs-size)")
	f.BoolVar(&o.noCache, "no-cache", false, "neither read nor write the cache")
	f.BoolVar(&o.details, "details", false, "add the assumptions, pins and analyzer diagnostics")
	f.BoolVar(&o.json, "json", false, "write the full report as JSON")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "enable debug logging on stderr")
	root.AddCommand(images)

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

// runImages sizes the images args name.
func runImages(ctx context.Context, args []string, o options, stdin io.Reader, stdout io.Writer, open opener) error {
	s, err := settings(o)
	if err != nil {
		return err
	}
	platform, err := ocilayout.ParsePlatform(s.Platform)
	if err != nil {
		return &cli.UsageError{Err: err}
	}
	refs, err := readRefs(args, stdin)
	if err != nil {
		return err
	}
	cacheDir := ""
	if !o.noCache {
		if cacheDir = o.cacheDir; cacheDir == "" {
			if cacheDir, err = defaultCacheDir(); err != nil {
				return err
			}
		}
	}
	metaDir := ""
	if cacheDir != "" {
		metaDir = filepath.Join(cacheDir, "manifests", "sha256")
	}
	sp, err := newSpool(metaDir)
	if err != nil {
		return err
	}
	defer sp.close()

	imgs, ls, err := loadImages(ctx, open, sp, cacheDir, refs, platform)
	if err != nil {
		return err
	}
	rep := buildReport(ctx, imgs, ls, s)
	for _, n := range sp.fetched {
		rep.Analyzer.BytesFetched += n
	}
	rep.Analyzer.BlobsFetched = len(sp.fetched)
	if o.json {
		return writeJSON(stdout, rep)
	}
	return writeText(stdout, rep, o.details)
}

// loadImages resolves every reference and loads each distinct layer once.
// An image named twice, by the same or another reference, is kept once.
func loadImages(ctx context.Context, open opener, sp *spool, cacheDir string, refs []string, platform ocispec.Platform) ([]*Image, *layerSet, error) {
	ls := newLayerSet(cacheDir)
	seen := map[string]bool{}
	var imgs []*Image
	for _, ref := range refs {
		img, src, err := resolveImage(ctx, open, sp, ref, platform)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", ref, err)
		}
		if seen[img.ManifestDigest] {
			continue
		}
		seen[img.ManifestDigest] = true
		for _, l := range img.Layers {
			if err := ls.load(ctx, src, l); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", ref, err)
			}
		}
		imgs = append(imgs, img)
	}
	return imgs, ls, nil
}

// settings turns the flags into the models' profile.
func settings(o options) (Settings, error) {
	s := Settings{
		Platform: o.platform,
		FS:       o.fs,
		Profile: Profile{
			HostBlock:      4096,
			ErofsBlock:     erofsBlock,
			VerityBlock:    4096,
			InlineMax:      o.inlineMax,
			InlineMaxLayer: 0,
		},
	}
	if s.Platform == "" {
		s.Platform = "linux/" + runtime.GOARCH
	}
	if o.inlineMax < minInlineMax || o.inlineMax > maxInlineMax {
		return s, &cli.UsageError{Err: fmt.Errorf("--inline-max must be between %d and %d bytes, not %d", minInlineMax, maxInlineMax, o.inlineMax)}
	}
	if o.inlineMax > composefsInlineCap {
		s.InlineMaxNote = inlineBeyondCap
	}
	if o.fs != "ext4" {
		return s, &cli.UsageError{Err: fmt.Errorf("--fs supports only ext4, not %q", o.fs)}
	}
	switch o.hostMetadata {
	case "include":
		s.Profile.HostMetadata = true
	case "exclude":
	default:
		return s, &cli.UsageError{Err: fmt.Errorf("--host-metadata must be include or exclude, not %q", o.hostMetadata)}
	}
	return s, nil
}
