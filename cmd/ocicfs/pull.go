// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/digestutil"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func newPullCommand(g *globalOptions) *cobra.Command {
	var fromDir string
	var fromTar string
	var delta bool
	var ingestWorkers int
	format := cli.FormatText

	cmd := &cobra.Command{
		Use:   "pull [--delta] [--ingest-workers <n>] (<ref> | --from-dir <dir> <ref> | --from-tar <archive> [<ref>])",
		Short: "fetch an image into the local store",
		Args:  cli.MaxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromDir != "" && fromTar != "" {
				return &cli.UsageError{Err: errors.New("--from-dir and --from-tar are mutually exclusive")}
			}
			if len(args) == 0 && fromTar == "" {
				return &cli.UsageError{Err: errors.New("a reference is required unless --from-tar names the image")}
			}
			cfg := g.config()
			cfg.IngestWorkers = ingestWorkers
			eng, err := ocicfs.New(cfg)
			if err != nil {
				return err
			}
			var ref string
			if len(args) == 1 {
				ref = args[0]
			}
			var opts []ocicfs.PullOption
			if fromDir != "" {
				opts = append(opts, ocicfs.WithFromDir(fromDir))
			}
			if fromTar != "" {
				src, err := ocilayout.OpenTar(fromTar)
				if err != nil {
					return err
				}
				defer func() {
					if err := src.Close(); err != nil {
						slog.Warn("pull: failed to close archive", "err", err)
					}
				}()
				index, err := src.IndexJSON()
				if err != nil {
					return err
				}
				if ref, err = archiveRef(index, ref); err != nil {
					return err
				}
				opts = append(opts, ocicfs.WithBlobSource(src))
			}
			if delta {
				opts = append(opts, ocicfs.WithDelta())
			}
			img, err := eng.Pull(cmd.Context(), ref, opts...)
			if err != nil {
				return err
			}
			return cli.WriteImage(cmd.OutOrStdout(), format, img)
		},
	}

	cmd.Flags().StringVar(&fromDir, "from-dir", "", "sideload from an OCI image layout directory instead of a registry")
	cmd.Flags().StringVar(&fromTar, "from-tar", "", "sideload from an uncompressed OCI archive (docker save, skopeo oci-archive) instead of a registry; without <ref>, the name the archive carries is recorded")
	cmd.Flags().BoolVar(&delta, "delta", false, "fetch only the files the store lacks from zstd:chunked layers; hold the previous release first (see docs/pull.md)")
	cmd.Flags().IntVar(&ingestWorkers, "ingest-workers", ocicfs.DefaultIngestWorkers, "layers scanned (and, from a registry, downloaded) concurrently; more than the default helps only on fast storage")
	cli.AddFormatFlag(cmd, &format)
	return cmd
}

// archiveRef settles the reference a pull from the archive records. A
// reference that carries a digest, or that is a bare digest, is returned
// unchanged. A bare name is pinned to the one manifest the archive's
// index.json lists. No reference at all takes the name of that one entry
// (see archiveName), with the tag or digest the name carries replaced by the
// entry's own digest, which is what the archive holds, spelled as the store
// records it. An archive holding several images needs the caller to pin one
// of them.
func archiveRef(index []byte, ref string) (string, error) {
	if strings.Contains(ref, "@") || strings.HasPrefix(ref, "sha256:") {
		return ref, nil
	}
	var idx ocispec.Index
	if err := json.Unmarshal(index, &idx); err != nil {
		return "", fmt.Errorf("parse the archive's index.json: %w", err)
	}
	switch len(idx.Manifests) {
	case 0:
		return "", errors.New("the archive's index.json lists no manifests")
	case 1:
	default:
		entries := make([]string, 0, len(idx.Manifests))
		for _, m := range idx.Manifests {
			s := m.Digest.String()
			if name := m.Annotations["io.containerd.image.name"]; name != "" {
				s += " (" + name + ")"
			}
			entries = append(entries, s)
		}
		return "", fmt.Errorf("the archive holds %d images: %s; pin one as <name>@<digest>", len(idx.Manifests), strings.Join(entries, ", "))
	}
	entry := idx.Manifests[0]
	if err := digestutil.ValidateSHA256(entry.Digest); err != nil {
		return "", fmt.Errorf("the archive's index.json entry: %w", err)
	}
	if ref == "" {
		named, err := archiveName(entry.Annotations)
		if err != nil {
			return "", err
		}
		return ocicfs.NormalizeRef(reference.TrimNamed(named).String() + "@" + entry.Digest.String())
	}
	return ref + "@" + entry.Digest.String(), nil
}

// archiveName returns the name an archive's index.json entry gives its
// image: the io.containerd.image.name annotation, the reference docker save
// was given, or else an org.opencontainers.image.ref.name annotation that
// holds a fully qualified reference, as skopeo writes the reference it was
// given. A ref.name that is not fully qualified does not name the image:
// docker save writes the bare tag there, and the tag "latest" read as a name
// is the image docker.io/library/latest.
func archiveName(annotations map[string]string) (reference.Named, error) {
	if name := annotations["io.containerd.image.name"]; name != "" {
		named, err := reference.ParseNormalizedNamed(name)
		if err != nil {
			return nil, fmt.Errorf("the archive names its image %q: %w; pass a reference", name, err)
		}
		return named, nil
	}
	if named, err := reference.ParseNamed(annotations[ocispec.AnnotationRefName]); err == nil {
		return named, nil
	}
	return nil, errors.New("the archive does not name its image (no io.containerd.image.name annotation, nor a fully qualified org.opencontainers.image.ref.name); pass a reference")
}
