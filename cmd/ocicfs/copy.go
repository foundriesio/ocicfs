// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/ocichain"
	"github.com/foundriesio/ocicfs/internal/ocilayout"
	"github.com/foundriesio/ocicfs/internal/ociremote"
)

func newCopyCommand(*globalOptions) *cobra.Command {
	var output string
	var platform string

	cmd := &cobra.Command{
		Use:   "copy -o <archive> [--platform os/arch[/variant]] <ref>...",
		Short: "write registry images to an archive for pull --from-tar and docker load",
		Long: "copy fetches each image from its registry and writes them all to one " +
			"uncompressed OCI image layout archive, keeping only the blobs of one " +
			"platform. An image whose root is an index is listed twice, by the index " +
			"and by the platform's manifest, so the archive serves a pin on either " +
			"digest. copy needs no store and no privilege; on a build host of another " +
			"architecture, name the device's with --platform.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return &cli.UsageError{Err: errors.New("--output is required")}
			}
			if len(args) == 0 {
				return &cli.UsageError{Err: errors.New("at least one reference is required")}
			}
			want := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
			if platform != "" {
				p, err := ocilayout.ParsePlatform(platform)
				if err != nil {
					return &cli.UsageError{Err: fmt.Errorf("--platform: %w", err)}
				}
				want = p
			}
			return copyArchive(cmd.Context(), cmd.OutOrStdout(), output, args, want)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "archive to write; replaced only once it is complete")
	cmd.Flags().StringVar(&platform, "platform", "", "platform to keep, as os/arch[/variant] (default: the host's)")
	return cmd
}

// copyImage is one image to copy: the registry source it is read from, its
// verified chain for the copied platform, and the index.json entries that
// name it in the archive.
type copyImage struct {
	src     ocichain.BlobSource
	res     ocichain.Resolved
	entries []ocispec.Descriptor
}

// copyArchive writes the images refs name, for the platform want, to an
// archive at out and prints each index.json entry it lists. Every image is
// resolved and verified before any layer is fetched, so a bad reference or a
// platform an image lacks fails before the archive is started. The archive
// is written beside out under a temporary name and renamed into place once
// complete, so out is never a partial archive.
func copyArchive(ctx context.Context, stdout io.Writer, out string, refs []string, want ocispec.Platform) error {
	imgs := make([]copyImage, 0, len(refs))
	var entries []ocispec.Descriptor
	named := make(map[string]digest.Digest)
	for _, ref := range refs {
		img, err := resolveCopy(ctx, ref, want)
		if err != nil {
			return err
		}
		imgs = append(imgs, img)
		for _, e := range img.entries {
			name := e.Annotations[ocispec.AnnotationRefName]
			if prev, ok := named[name]; ok {
				if prev != e.Digest {
					return fmt.Errorf("copy: %s names both %s and %s", name, prev, e.Digest)
				}
				continue
			}
			named[name] = e.Digest
			entries = append(entries, e)
		}
		slog.Info("copy: resolved image", "ref", ref, "root", img.res.RootDesc.Digest, "manifest", img.res.ManifestDesc.Digest)
	}

	f, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*")
	if err != nil {
		return fmt.Errorf("copy: create archive: %w", err)
	}
	tmp := f.Name()
	err = f.Chmod(0o644)
	if err == nil {
		err = writeArchive(ctx, f, imgs, entries)
	}
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("copy: close archive: %w", closeErr)
	}
	if err == nil {
		err = os.Rename(tmp, out)
	}
	if err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			slog.Warn("copy: failed to remove partial archive", "path", tmp, "err", rmErr)
		}
		return err
	}
	for _, e := range entries {
		if _, err := fmt.Fprintf(stdout, "%s\t%s\n", e.Annotations[ocispec.AnnotationRefName], e.Digest); err != nil {
			return err
		}
	}
	return nil
}

// resolveCopy opens ref's registry and verifies its chain for the platform
// want, and returns it with the index.json entries that name it.
func resolveCopy(ctx context.Context, ref string, want ocispec.Platform) (copyImage, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return copyImage{}, fmt.Errorf("copy: parse reference %q: %w", ref, err)
	}
	src, root, err := ociremote.Open(ctx, ref)
	if err != nil {
		return copyImage{}, err
	}
	res, err := ocichain.Resolve(ctx, src, root, want)
	if err != nil {
		return copyImage{}, fmt.Errorf("copy %s: %w", ref, err)
	}
	entries, err := copyEntries(named, res)
	if err != nil {
		return copyImage{}, fmt.Errorf("copy %s: %w", ref, err)
	}
	return copyImage{src: src, res: res, entries: entries}, nil
}

// copyEntries returns the index.json entries that name the image res in the
// archive. The root is named by the reference it was copied by: its tag, or
// for a reference that carries only a digest, that digest. An index root is
// listed a second time by its platform manifest, named by that manifest's
// digest. A runtime importing the archive keys its images by name, so the
// two entries need distinct names, and either digest then resolves to an
// image. Both name annotations carry the full name, for the readers of either.
func copyEntries(named reference.Named, res ocichain.Resolved) ([]ocispec.Descriptor, error) {
	repo := reference.TrimNamed(named)
	rootName := repo.String() + "@" + res.RootDesc.Digest.String()
	if tagged, ok := named.(reference.Tagged); ok {
		rootName = repo.String() + ":" + tagged.Tag()
	} else if _, ok := named.(reference.Digested); !ok {
		rootName = repo.String() + ":latest"
	}
	isIndex, err := ocichain.IsIndex(res.RootBytes)
	if err != nil {
		return nil, err
	}
	mediaType := ocispec.MediaTypeImageManifest
	if isIndex {
		mediaType = ocispec.MediaTypeImageIndex
	}
	var doc struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(res.RootBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse root: %w", err)
	}
	if doc.MediaType != "" {
		mediaType = doc.MediaType
	}
	entries := []ocispec.Descriptor{{
		MediaType:   mediaType,
		Digest:      res.RootDesc.Digest,
		Size:        res.RootDesc.Size,
		Annotations: imageNames(rootName),
	}}
	if isIndex {
		m := res.ManifestDesc
		m.Annotations = imageNames(repo.String() + "@" + m.Digest.String())
		entries = append(entries, m)
	}
	return entries, nil
}

// imageNames returns the annotations that name an image in an archive's
// index.json: the containerd one docker load reads and the OCI one.
func imageNames(name string) map[string]string {
	return map[string]string{
		"io.containerd.image.name": name,
		ocispec.AnnotationRefName:  name,
	}
}

// writeArchive writes an OCI image layout as a tar to w: the layout marker,
// each image's root, manifest, config and layer blobs, each written once,
// and an index.json listing entries. Metadata blobs are written from their
// verified bytes; layers are streamed from the registry and verified as they
// pass, so a corrupt or short layer fails the write.
func writeArchive(ctx context.Context, w io.Writer, imgs []copyImage, entries []ocispec.Descriptor) error {
	tw := tar.NewWriter(w)
	layout, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, ocispec.ImageLayoutFile, int64(len(layout)), bytes.NewReader(layout)); err != nil {
		return err
	}
	written := make(map[digest.Digest]bool)
	writeBytes := func(d digest.Digest, b []byte) error {
		if written[d] {
			return nil
		}
		written[d] = true
		return writeTarFile(tw, blobPath(d), int64(len(b)), bytes.NewReader(b))
	}
	for _, img := range imgs {
		res := img.res
		if err := writeBytes(res.RootDesc.Digest, res.RootBytes); err != nil {
			return err
		}
		if err := writeBytes(res.ManifestDesc.Digest, res.ManifestBytes); err != nil {
			return err
		}
		if err := writeBytes(res.ConfigDesc.Digest, res.ConfigBytes); err != nil {
			return err
		}
		for _, l := range res.Manifest.Layers {
			if written[l.Digest] {
				continue
			}
			written[l.Digest] = true
			if err := writeLayer(ctx, tw, img.src, l); err != nil {
				return err
			}
			slog.Debug("copy: wrote layer", "digest", l.Digest, "size", l.Size)
		}
	}
	index, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: entries,
	})
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, ocispec.ImageIndexFile, int64(len(index)), bytes.NewReader(index)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("copy: finish archive: %w", err)
	}
	return nil
}

// writeLayer streams the layer blob desc from src into the archive.
func writeLayer(ctx context.Context, tw *tar.Writer, src ocichain.BlobSource, desc ocispec.Descriptor) error {
	rc, err := ocichain.OpenBlob(ctx, src, desc)
	if err != nil {
		return fmt.Errorf("copy: open layer %s: %w", desc.Digest, err)
	}
	err = writeTarFile(tw, blobPath(desc.Digest), desc.Size, rc)
	if closeErr := rc.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("copy: close layer %s: %w", desc.Digest, closeErr)
	}
	return err
}

// writeTarFile writes a regular file of size bytes read from r to tw.
func writeTarFile(tw *tar.Writer, name string, size int64, r io.Reader) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     size,
		ModTime:  time.Unix(0, 0),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("copy: write %s: %w", name, err)
	}
	if _, err := io.Copy(tw, r); err != nil {
		return fmt.Errorf("copy: write %s: %w", name, err)
	}
	return nil
}

// blobPath returns the archive path of the blob d.
func blobPath(d digest.Digest) string {
	return "blobs/" + d.Algorithm().String() + "/" + d.Encoded()
}
