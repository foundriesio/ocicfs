// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package ociseal computes composefs integrity annotations in OCI image
// manifests.
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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/composefs"
	"github.com/foundriesio/knarr/internal/digestutil"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ocichain"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/rootfs"
	"github.com/foundriesio/knarr/internal/verity"
)

const (
	// MergedAnnotation records the fs-verity digest of the merged EROFS.
	MergedAnnotation = "composefs.merged.erofs.v1.fsverity-sha256-12"
	// ConfigAnnotation records the fs-verity digest of the config JSON bytes.
	ConfigAnnotation = "composefs.config.fsverity-sha256-12"

	// FallbackMergedAnnotation records the merged fs-verity digest of a
	// non-canonical (fallback) construction. It lives in knarr's own
	// namespace because the shared namespace above promises a digest the
	// upstream canonical algorithm reproduces, which a fallback digest is
	// not.
	FallbackMergedAnnotation = "io.foundries.knarr.composefs.fallback.v1.fsverity-sha256-12"
	// FallbackConfigAnnotation is the config-digest companion of
	// FallbackMergedAnnotation.
	FallbackConfigAnnotation = "io.foundries.knarr.composefs.config.fsverity-sha256-12"
)

// ErrNotCanonical reports that an image's composefs construction diverges from
// the upstream-reproducible form, so its merged EROFS digest is not a stable
// value to seal. It is wrapped platform-qualified when returned.
var ErrNotCanonical = errors.New("ociseal: construction is not upstream-canonical")

// Result describes a computed sealed root.
type Result struct {
	Original  digest.Digest
	Sealed    digest.Digest
	Platforms []PlatformResult
	// Repository is the fully qualified repository associated with the
	// result. Compute leaves it empty because a blob source carries no
	// repository identity.
	Repository string
}

// Manifest is an OCI manifest or index produced by Compute.
type Manifest struct {
	Raw       []byte
	MediaType string
	Digest    digest.Digest
	// Changed reports whether the manifest must be pushed by digest. An
	// unchanged manifest is returned only as the root that may be tagged.
	Changed bool
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

// Compute returns the manifests in push order, with the root last. It computes
// integrity annotations for root directly, or for the child selected by want
// when root is an image index.
func Compute(ctx context.Context, src ocichain.BlobSource, root digest.Digest, want *ocispec.Platform, allowFallback bool) (Result, []Manifest, error) {
	// Sealing builds the merged EROFS with mkcomposefs after reading every
	// layer; check the binary up front so a missing one fails immediately.
	if err := composefs.Preflight(); err != nil {
		return Result{}, nil, err
	}
	return seal(ctx, src, root, want, allowFallback)
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
	sealedManifest struct {
		raw            []byte
		descriptor     ocispec.Descriptor
		platform       ocispec.Platform
		mergedFsverity string
		changed        bool
	}

	hashSink struct{}
)

func (hashSink) WriteObject(ctx context.Context, r io.Reader) (digest.Digest, objectstore.Sealed, error) {
	h := sha256.New()
	fsv, err := verity.Digest(ctx, io.TeeReader(r, h))
	if err != nil {
		return "", nil, fmt.Errorf("ociseal: hash object: %w", err)
	}
	return digest.NewDigest(digest.SHA256, h), objectstore.Resolved(fsv), nil
}

func seal(ctx context.Context, src ocichain.BlobSource, root digest.Digest, want *ocispec.Platform, allowFallback bool) (Result, []Manifest, error) {
	rootRaw, err := ocichain.ReadRootBlob(ctx, src, root)
	if err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: read root: %w", err)
	}
	isIndex, err := ocichain.IsIndex(rootRaw)
	if err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: parse root: %w", err)
	}
	if isIndex {
		if want == nil {
			return Result{}, nil, errors.New("ociseal: the reference names an image index; select one platform with --platform os/arch[/variant]")
		}
		return sealIndexPlatform(ctx, src, root, rootRaw, *want, allowFallback)
	}
	if want != nil {
		return Result{}, nil, errors.New("ociseal: the reference names an image manifest; --platform applies only to an index")
	}
	return sealSingle(ctx, src, root, rootRaw, allowFallback)
}

func sealSingle(ctx context.Context, src ocichain.BlobSource, root digest.Digest, rootRaw []byte, allowFallback bool) (Result, []Manifest, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(rootRaw, &manifest); err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: parse root manifest: %w", err)
	}
	if !isImageManifest(manifest) {
		return Result{}, nil, errors.New("ociseal: root is not a container image manifest (no config, or layers that are not tar archives)")
	}
	want, err := manifestPlatform(ctx, src, manifest.Config)
	if err != nil {
		return Result{}, nil, err
	}
	sealed, err := sealPlatform(ctx, src, root, want, allowFallback)
	if err != nil {
		return Result{}, nil, err
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
	return result, []Manifest{{
		Raw:       sealed.raw,
		MediaType: sealed.descriptor.MediaType,
		Digest:    result.Sealed,
		Changed:   sealed.changed,
	}}, nil
}

// sealIndexPlatform seals exactly one platform of an index: the child want
// selects. That manifest is sealed and its index descriptor rewritten in
// place; every other child — other platforms, attestation manifests, vendor
// artifacts — is left byte-identical and never even fetched. The returned
// sealed digest is the updated index's, so a multi-platform index is fully
// sealed by chaining calls, each feeding the previous call's output digest.
func sealIndexPlatform(ctx context.Context, src ocichain.BlobSource, root digest.Digest, rootRaw []byte, want ocispec.Platform, allowFallback bool) (Result, []Manifest, error) {
	var index ocispec.Index
	if err := json.Unmarshal(rootRaw, &index); err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: parse index: %w", err)
	}
	// The generic document carries the members the typed index drops, so
	// rewriting one child's descriptor in it leaves every other member — and
	// every other child — as the publisher wrote it.
	rootDoc, err := decodeDocument(rootRaw)
	if err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: parse root: %w", err)
	}
	rawDescriptors, ok := rootDoc["manifests"].([]any)
	if !ok || len(rawDescriptors) != len(index.Manifests) {
		return Result{}, nil, errors.New("ociseal: index manifests has invalid shape")
	}

	// Selection is exact on os/arch/variant as the index descriptors declare
	// them: a request that matches nothing or more than one child is an error
	// naming the candidates, never a silent first match — real indexes carry
	// several children differing only in variant.
	child := -1
	for i, desc := range index.Manifests {
		if desc.Platform == nil || !platformsEqual(*desc.Platform, want) {
			continue
		}
		if child >= 0 {
			return Result{}, nil, fmt.Errorf("ociseal: platform %s matches more than one manifest in the index; qualify it (os/arch/variant)", scrubPlatform(&want))
		}
		child = i
	}
	if child < 0 {
		return Result{}, nil, fmt.Errorf("ociseal: no manifest for platform %s in the index; it provides: %s", scrubPlatform(&want), indexPlatforms(index))
	}

	childRaw, err := ocichain.ReadMetaBlob(ctx, src, index.Manifests[child])
	if err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: read manifest for platform %s: %w", scrubPlatform(&want), err)
	}
	nested, err := ocichain.IsIndex(childRaw)
	if err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: parse manifest for platform %s: %w", scrubPlatform(&want), err)
	}
	if nested {
		return Result{}, nil, fmt.Errorf("ociseal: platform %s names a nested index: recursive sealing is not supported", scrubPlatform(&want))
	}
	manifest, err := sealPlatform(ctx, src, index.Manifests[child].Digest, want, allowFallback)
	if err != nil {
		return Result{}, nil, err
	}

	result := Result{Original: root, Sealed: root, Platforms: []PlatformResult{{
		Platform:       manifest.platform,
		Original:       index.Manifests[child].Digest,
		Sealed:         manifest.descriptor.Digest,
		MergedFsverity: manifest.mergedFsverity,
	}}}
	if !manifest.changed {
		return result, []Manifest{{
			Raw:       rootRaw,
			MediaType: documentMediaType(rootDoc, ocispec.MediaTypeImageIndex),
			Digest:    root,
		}}, nil
	}

	rawDesc, ok := rawDescriptors[child].(map[string]any)
	if !ok {
		return Result{}, nil, fmt.Errorf("ociseal: index manifest %d has invalid shape", child)
	}
	rawDesc["digest"] = manifest.descriptor.Digest.String()
	rawDesc["size"] = json.Number(fmt.Sprint(manifest.descriptor.Size))
	rewritten, err := json.Marshal(rootDoc)
	if err != nil {
		return Result{}, nil, fmt.Errorf("ociseal: marshal index: %w", err)
	}
	result.Sealed = digest.FromBytes(rewritten)
	mediaType := documentMediaType(rootDoc, ocispec.MediaTypeImageIndex)
	return result, []Manifest{
		{
			Raw:       manifest.raw,
			MediaType: manifest.descriptor.MediaType,
			Digest:    manifest.descriptor.Digest,
			Changed:   true,
		},
		{
			Raw:       rewritten,
			MediaType: mediaType,
			Digest:    result.Sealed,
			Changed:   true,
		},
	}, nil
}

// platformsEqual reports whether two platforms name the same os/arch/variant.
// OSVersion and OSFeatures are ignored: index descriptors rarely carry them
// and a seal caller cannot reasonably be required to reproduce them.
func platformsEqual(a, b ocispec.Platform) bool {
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant
}

// indexPlatforms renders the platforms an index declares, for error messages.
func indexPlatforms(index ocispec.Index) string {
	var out []string
	for _, desc := range index.Manifests {
		if desc.Platform == nil {
			continue
		}
		out = append(out, scrubPlatform(desc.Platform))
	}
	if len(out) == 0 {
		return "no declared platforms"
	}
	return strings.Join(out, ", ")
}

func sealPlatform(ctx context.Context, src ocichain.BlobSource, root digest.Digest, want ocispec.Platform, allowFallback bool) (sealedManifest, error) {
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
	if !canonical && !allowFallback {
		return sealedManifest{}, fmt.Errorf("ociseal: platform %s: %w (pass --allow-non-canonical to seal the fallback construction under the knarr annotations)", scrubPlatform(&res.Platform), ErrNotCanonical)
	}
	merged, err := mergedDigest(ctx, entries)
	if err != nil {
		return sealedManifest{}, err
	}
	config, err := verity.Digest(ctx, bytes.NewReader(res.ConfigBytes))
	if err != nil {
		return sealedManifest{}, fmt.Errorf("ociseal: digest config: %w", err)
	}
	raw, changed, err := annotateManifest(res.ManifestBytes, merged, config, canonical)
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

// annotateManifest records merged and config under the standard annotations
// for a canonical construction and under the knarr fallback annotations
// otherwise: the standard namespace must only ever carry digests the upstream
// algorithm reproduces. A non-canonical manifest that already carries any
// standard merged annotation is refused — such an image is rejected at pull
// time, and sealing must not launder it.
func annotateManifest(raw []byte, merged, config string, canonical bool) ([]byte, bool, error) {
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
	mergedKey, configKey := MergedAnnotation, ConfigAnnotation
	if !canonical {
		mergedKey, configKey = FallbackMergedAnnotation, FallbackConfigAnnotation
		for key := range annotations {
			if strings.HasPrefix(key, MergedAnnotationPrefix) {
				return nil, false, fmt.Errorf("ociseal: manifest carries %s but the construction is not canonical", scrubControl(key))
			}
		}
	}
	mergedPresent, err := checkExisting(annotations, mergedKey, merged)
	if err != nil {
		return nil, false, err
	}
	configPresent, err := checkExisting(configAnnotations, configKey, config)
	if err != nil {
		return nil, false, err
	}
	if mergedPresent && configPresent {
		return raw, false, nil
	}
	annotations[mergedKey] = mergedValue
	configAnnotations[configKey] = configValue
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
	return scrubControl(ocilayout.PlatformString(p))
}

// scrubControl replaces control characters in s with spaces, so a value
// derived from untrusted input cannot inject escape sequences into terminal
// output.
func scrubControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func documentMediaType(doc map[string]any, fallback string) string {
	if mediaType, ok := doc["mediaType"].(string); ok && mediaType != "" {
		return mediaType
	}
	return fallback
}
