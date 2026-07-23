// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ociseal computes and records composefs integrity annotations in OCI
// image manifests.
package ociseal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/ociremote"
	"github.com/foundriesio/knarr/internal/rootfs"
	"github.com/foundriesio/knarr/internal/verity"
)

const (
	// MergedAnnotation records the fs-verity digest of the merged EROFS.
	MergedAnnotation = "composefs.merged.erofs.v1.fsverity-sha256-12"
	// ConfigAnnotation records the fs-verity digest of the config JSON bytes.
	ConfigAnnotation = "composefs.config.fsverity-sha256-12"
)

// ErrNotCanonical reports that an image's composefs construction diverges from
// the upstream-reproducible form, so its merged EROFS digest is not a stable
// value to seal. It is wrapped platform-qualified when returned.
var ErrNotCanonical = errors.New("ociseal: construction is not upstream-canonical")

// Options controls what Seal targets and which registry writes it performs.
type Options struct {
	Tag    string
	DryRun bool
}

// Result describes the root produced by Seal.
type Result struct {
	Original  digest.Digest
	Sealed    digest.Digest
	Platforms []PlatformResult
	// Repository is the fully qualified repository the sealed manifests were
	// pushed to, so a caller can render every digest here as a pullable
	// reference. It is the input reference's repository: seal never writes
	// outside it.
	Repository string
}

// PlatformResult describes one sealed platform manifest. Original is the
// manifest the input named for this platform — the index child's descriptor
// digest, or the root itself when the reference named a manifest — and Sealed
// is what it became; they are equal when the platform was already sealed.
type PlatformResult struct {
	Platform       ocispec.Platform
	Original       digest.Digest
	Sealed         digest.Digest
	MergedFsverity string
}

// Seal computes integrity annotations for the image manifest ref names and
// pushes the changed manifest in the same repository unless DryRun is set.
func Seal(ctx context.Context, ref string, opts Options) (Result, error) {
	src, root, err := ociremote.Open(ctx, ref)
	if err != nil {
		return Result{}, err
	}
	input, err := name.NewDigest(ref)
	if err != nil {
		return Result{}, fmt.Errorf("ociseal: parse reference: %w", err)
	}
	tag, err := outputTag(input.Context(), opts.Tag)
	if err != nil {
		return Result{}, err
	}
	result, err := seal(ctx, src, input.Context(), root, tag, opts.DryRun)
	if err != nil {
		return Result{}, err
	}
	result.Repository = input.Context().String()
	return result, nil
}

// ParseAnnotation strictly converts a bare lowercase sha256 annotation value
// into the internal "sha256:<hex>" form.
func ParseAnnotation(value string) (string, error) {
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("annotation must contain %d lowercase hex characters", sha256.Size*2)
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", errors.New("annotation must be bare lowercase hex")
		}
	}
	d := digest.Digest("sha256:" + value)
	if err := digestutil.ValidateSHA256(d); err != nil {
		return "", fmt.Errorf("annotation digest: %w", err)
	}
	return d.String(), nil
}

// AnnotationValue strictly converts an internal sha256 digest into the bare
// lowercase hex form used by sealing annotations.
func AnnotationValue(value string) (string, error) {
	d, err := digestutil.ParseSHA256(value)
	if err != nil {
		return "", fmt.Errorf("annotation digest: %w", err)
	}
	return d.Encoded(), nil
}

type (
	rawManifest struct {
		data      []byte
		mediaType types.MediaType
	}

	sealedManifest struct {
		raw            []byte
		descriptor     ocispec.Descriptor
		platform       ocispec.Platform
		mergedFsverity string
		changed        bool
	}

	hashSink struct{}
)

func (m rawManifest) RawManifest() ([]byte, error) {
	return m.data, nil
}

func (m rawManifest) MediaType() (types.MediaType, error) {
	return m.mediaType, nil
}

func (hashSink) WriteObject(ctx context.Context, r io.Reader) (digest.Digest, string, error) {
	h := sha256.New()
	fsv, err := verity.Digest(ctx, io.TeeReader(r, h))
	if err != nil {
		return "", "", fmt.Errorf("ociseal: hash object: %w", err)
	}
	return digest.NewDigest(digest.SHA256, h), fsv, nil
}

func seal(ctx context.Context, src ocichain.BlobSource, repo name.Repository, root digest.Digest, tag *name.Tag, dryRun bool) (Result, error) {
	rootRaw, err := ocichain.ReadRootBlob(ctx, src, root)
	if err != nil {
		return Result{}, fmt.Errorf("ociseal: read root: %w", err)
	}
	isIndex, err := ocichain.IsIndex(rootRaw)
	if err != nil {
		return Result{}, fmt.Errorf("ociseal: parse root: %w", err)
	}
	if isIndex {
		return Result{}, errors.New("ociseal: the reference names an image index; seal targets one image manifest, named by its digest")
	}
	return sealSingle(ctx, src, repo, root, rootRaw, tag, dryRun)
}

func sealSingle(ctx context.Context, src ocichain.BlobSource, repo name.Repository, root digest.Digest, rootRaw []byte, tag *name.Tag, dryRun bool) (Result, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(rootRaw, &manifest); err != nil {
		return Result{}, fmt.Errorf("ociseal: parse root manifest: %w", err)
	}
	if !isImageManifest(manifest) {
		return Result{}, errors.New("ociseal: root is not a container image manifest (no config, or layers that are not tar archives)")
	}
	want, err := manifestPlatform(ctx, src, manifest.Config)
	if err != nil {
		return Result{}, err
	}
	sealed, err := sealPlatform(ctx, src, root, want)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Original: root,
		Sealed:   sealed.descriptor.Digest,
		Platforms: []PlatformResult{{
			Platform:       sealed.platform,
			Original:       root,
			Sealed:         sealed.descriptor.Digest,
			MergedFsverity: sealed.mergedFsverity,
		}},
	}
	if !sealed.changed {
		result.Sealed = root
	}
	if dryRun {
		return result, nil
	}
	if sealed.changed {
		if err := putManifest(ctx, repo.Digest(sealed.descriptor.Digest.String()), sealed.raw, sealed.descriptor.MediaType); err != nil {
			return Result{}, fmt.Errorf("ociseal: push manifest: %w", err)
		}
	}
	if tag != nil {
		// An unchanged manifest is not re-pushed, but a requested tag still
		// lands on it: sealed.raw carries the original bytes in that case.
		if err := putManifest(ctx, *tag, sealed.raw, sealed.descriptor.MediaType); err != nil {
			return Result{}, fmt.Errorf("ociseal: push tag: %w", err)
		}
	}
	return result, nil
}

func sealPlatform(ctx context.Context, src ocichain.BlobSource, root digest.Digest, want ocispec.Platform) (sealedManifest, error) {
	res, err := ocichain.Resolve(ctx, src, root, want)
	if err != nil {
		return sealedManifest{}, err
	}
	if !isImageManifest(res.Manifest) {
		return sealedManifest{}, fmt.Errorf("ociseal: platform %s is not a container image manifest (no config, or layers that are not tar archives)", scrubPlatform(&want))
	}
	openLayer := func(i int) (io.ReadCloser, error) {
		return ocichain.OpenLayer(ctx, src, res.Manifest.Layers[i])
	}
	entries, canonical, err := rootfs.BuildEntries(ctx, hashSink{}, openLayer, res.DiffIDs)
	if err != nil {
		return sealedManifest{}, fmt.Errorf("ociseal: build entries: %w", err)
	}
	if !canonical {
		return sealedManifest{}, fmt.Errorf("ociseal: platform %s: %w", scrubPlatform(&res.Platform), ErrNotCanonical)
	}
	merged, err := mergedDigest(ctx, entries)
	if err != nil {
		return sealedManifest{}, err
	}
	config, err := verity.Digest(ctx, bytes.NewReader(res.ConfigBytes))
	if err != nil {
		return sealedManifest{}, fmt.Errorf("ociseal: digest config: %w", err)
	}
	raw, changed, err := annotateManifest(res.ManifestBytes, merged, config)
	if err != nil {
		return sealedManifest{}, err
	}
	mediaType := res.Manifest.MediaType
	if mediaType == "" {
		mediaType = res.ManifestDesc.MediaType
	}
	if mediaType == "" {
		mediaType = ocispec.MediaTypeImageManifest
	}
	return sealedManifest{
		raw: raw,
		descriptor: ocispec.Descriptor{
			MediaType: mediaType,
			Digest:    digest.FromBytes(raw),
			Size:      int64(len(raw)),
		},
		platform:       res.Platform,
		mergedFsverity: merged,
		changed:        changed,
	}, nil
}

func mergedDigest(ctx context.Context, entries []composefs.Entry) (string, error) {
	dir, err := os.MkdirTemp("", "knarr-seal-*")
	if err != nil {
		return "", fmt.Errorf("ociseal: create temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("ociseal: failed to remove temp directory", "path", dir, "err", err)
		}
	}()

	out := filepath.Join(dir, "merged.erofs")
	if err := composefs.WriteImage(ctx, out, entries); err != nil {
		return "", fmt.Errorf("ociseal: write composefs image: %w", err)
	}
	f, err := os.Open(out) // #nosec G304 -- path is inside the seal-owned temp directory
	if err != nil {
		return "", fmt.Errorf("ociseal: open composefs image: %w", err)
	}
	fsv, digestErr := verity.Digest(ctx, f)
	closeErr := f.Close()
	if digestErr != nil {
		digestErr = fmt.Errorf("ociseal: digest composefs image: %w", digestErr)
		if closeErr != nil {
			return "", errors.Join(digestErr, fmt.Errorf("ociseal: close composefs image: %w", closeErr))
		}
		return "", digestErr
	}
	if closeErr != nil {
		return "", fmt.Errorf("ociseal: close composefs image: %w", closeErr)
	}
	return fsv, nil
}

func annotateManifest(raw []byte, merged, config string) ([]byte, bool, error) {
	doc, err := decodeDocument(raw)
	if err != nil {
		return nil, false, fmt.Errorf("ociseal: parse manifest: %w", err)
	}
	mergedValue, err := AnnotationValue(merged)
	if err != nil {
		return nil, false, err
	}
	configValue, err := AnnotationValue(config)
	if err != nil {
		return nil, false, err
	}

	annotations, err := annotationMap(doc, "manifest")
	if err != nil {
		return nil, false, err
	}
	configDoc, ok := doc["config"].(map[string]any)
	if !ok {
		return nil, false, errors.New("ociseal: manifest config has invalid shape")
	}
	configAnnotations, err := annotationMap(configDoc, "config descriptor")
	if err != nil {
		return nil, false, err
	}
	mergedPresent, err := checkExisting(annotations, MergedAnnotation, merged)
	if err != nil {
		return nil, false, err
	}
	configPresent, err := checkExisting(configAnnotations, ConfigAnnotation, config)
	if err != nil {
		return nil, false, err
	}
	if mergedPresent && configPresent {
		return raw, false, nil
	}
	annotations[MergedAnnotation] = mergedValue
	configAnnotations[ConfigAnnotation] = configValue
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return nil, false, fmt.Errorf("ociseal: marshal manifest: %w", err)
	}
	return rewritten, true, nil
}

func annotationMap(doc map[string]any, location string) (map[string]any, error) {
	value, ok := doc["annotations"]
	if !ok {
		annotations := make(map[string]any)
		doc["annotations"] = annotations
		return annotations, nil
	}
	annotations, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ociseal: %s annotations has invalid shape", location)
	}
	return annotations, nil
}

func checkExisting(annotations map[string]any, key, expected string) (bool, error) {
	value, ok := annotations[key]
	if !ok {
		return false, nil
	}
	text, ok := value.(string)
	if !ok {
		return false, fmt.Errorf("ociseal: annotation %s is not a string", key)
	}
	parsed, err := ParseAnnotation(text)
	if err != nil {
		return false, fmt.Errorf("ociseal: annotation %s: %w", key, err)
	}
	if parsed != expected {
		return false, fmt.Errorf("ociseal: annotation %s mismatch: recorded %s, computed %s", key, parsed, expected)
	}
	return true, nil
}

func manifestPlatform(ctx context.Context, src ocichain.BlobSource, desc ocispec.Descriptor) (ocispec.Platform, error) {
	raw, err := ocichain.ReadMetaBlob(ctx, src, desc)
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("ociseal: read config: %w", err)
	}
	var config ocispec.Image
	if err := json.Unmarshal(raw, &config); err != nil {
		return ocispec.Platform{}, fmt.Errorf("ociseal: parse config: %w", err)
	}
	if config.OS == "" || config.Architecture == "" {
		return ocispec.Platform{}, errors.New("ociseal: config has no platform")
	}
	return config.Platform, nil
}

func decodeDocument(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON document")
	}
	return doc, nil
}

// imageLayerTypes are the tar-archive layer media types a container image
// manifest carries on a registry: the OCI layer types and the Docker schema2
// equivalents.
var imageLayerTypes = map[string]bool{
	ocispec.MediaTypeImageLayer:                                    true,
	ocispec.MediaTypeImageLayerGzip:                                true,
	ocispec.MediaTypeImageLayerZstd:                                true,
	"application/vnd.oci.image.layer.nondistributable.v1.tar":      true,
	"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip": true,
	"application/vnd.oci.image.layer.nondistributable.v1.tar+zstd": true,
	"application/vnd.docker.image.rootfs.diff.tar.gzip":            true,
	"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":    true,
}

// isImageManifest reports whether m is a container image manifest sealing can
// process: it carries a config and every layer is a tar archive. Config
// presence alone cannot tell an image from an attestation — a Docker Hub /
// buildx attestation manifest carries a regular image config and
// image-manifest media type, and only its layer types (in-toto JSON, not tar)
// reveal that it holds no filesystem to seal. Layers is nil only when the
// manifest omits the layers member the image spec requires; an empty Layers is
// a valid image with an empty rootfs.
func isImageManifest(m ocispec.Manifest) bool {
	if m.Config.Digest == "" || m.Layers == nil {
		return false
	}
	for _, layer := range m.Layers {
		if !imageLayerTypes[layer.MediaType] {
			return false
		}
	}
	return true
}

// scrubPlatform renders a platform as a display string with control characters
// replaced by spaces, so an os/arch/variant derived from publisher-controlled
// config JSON cannot inject escape sequences into terminal error output.
func scrubPlatform(p *ocispec.Platform) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ocilayout.PlatformString(p))
}

func putManifest(ctx context.Context, ref name.Reference, raw []byte, mediaType string) error {
	return remote.Put(ref, rawManifest{data: raw, mediaType: types.MediaType(mediaType)},
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
}

func outputTag(repo name.Repository, value string) (*name.Tag, error) {
	if value == "" {
		return nil, nil
	}
	if strings.ContainsAny(value, "/@") {
		return nil, errors.New("ociseal: --tag must be a tag name in the input repository")
	}
	tag, err := name.NewTag(repo.Name() + ":" + value)
	if err != nil {
		return nil, fmt.Errorf("ociseal: parse --tag: %w", err)
	}
	return &tag, nil
}
