// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrlayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/octest"
)

// The object-store payload carried by canonical seal fixtures is larger than the
// inline threshold, so it is stored by digest and exercises the publisher's
// per-file verity path; a fixture also carries an inline file and an empty file.
const sealObjectBody = "sealed object payload well beyond the sixty-four byte inline threshold, stored by digest\n"

// TestSealPullMountE2E exercises the full publisher-to-device flow on a
// verity-capable store: push a canonical image, seal it, pull the sealed digest,
// confirm the device fs-verity digest equals the publisher's computed merged
// digest, mount the result, and read back the object, inline, and empty files.
// A nested subtest ties the published merged annotation directly to
// `cfsctl oci compute-id`, so a missing cfsctl skips only that assertion.
func TestSealPullMountE2E(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay := buildCanonicalSealLayout(t, hostPlatform(), sealObjectBody)
	pushLayoutImage(t, registry, "registry.example.com/team/sealed:input", lay)

	input := "registry.example.com/team/sealed@" + lay.Root.String()
	sealed, err := ociseal.Seal(context.Background(), input, ociseal.Options{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ref := "registry.example.com/team/sealed@" + sealed.Sealed.String()

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.FsverityDigest != sealed.Platforms[0].MergedFsverity {
		t.Fatalf("device fs-verity = %s, publisher computed %s", info.FsverityDigest, sealed.Platforms[0].MergedFsverity)
	}

	// The published merged annotation, read back from the registry rather than
	// the Seal result, must equal the digest cfsctl computes from the same
	// layers. This proves the sealed digest is upstream-reproducible directly,
	// where before it held only transitively through the conformance gates.
	t.Run("cross-tool-oracle", func(t *testing.T) {
		requireOracleTools(t)
		published := publishedMergedAnnotation(t, registry, ref)
		cfsRepo := initOracleRepo(t, storeRoot)
		runCfsctl(t, "--repo", cfsRepo, "oci", "pull", "oci:"+lay.Dir, oracleTag)
		oracle := strings.TrimSpace(runCfsctl(t, "--repo", cfsRepo, "oci", "compute-id", oracleTag))
		if published != oracle {
			t.Fatalf("published merged annotation %s != cfsctl compute-id %s", published, oracle)
		}
	})

	if _, err := exec.LookPath("mount.composefs"); err != nil {
		t.Skip("mount.composefs not found in PATH")
	}
	target := filepath.Join(storeRoot, "sealed-mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	mounted := false
	t.Cleanup(func() {
		if mounted {
			if err := eng.Unmount(context.Background(), target); err != nil {
				t.Errorf("cleanup unmount: %v", err)
			}
		}
	})
	if err := eng.Mount(context.Background(), ref, target); skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	} else if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	mounted = true

	if data, err := os.ReadFile(filepath.Join(target, "usr", "bin", "tool")); err != nil { // #nosec G304 -- test reads the rootfs it mounted
		t.Fatalf("read object file: %v", err)
	} else if string(data) != sealObjectBody {
		t.Fatalf("object file = %q", data)
	}
	if data, err := os.ReadFile(filepath.Join(target, "etc", "config.txt")); err != nil { // #nosec G304 -- test reads the rootfs it mounted
		t.Fatalf("read inline file: %v", err)
	} else if string(data) != "sealed image\n" {
		t.Fatalf("inline file = %q", data)
	}
	if st, err := os.Stat(filepath.Join(target, "empty")); err != nil {
		t.Fatalf("stat empty file: %v", err)
	} else if st.Size() != 0 {
		t.Fatalf("empty file size = %d", st.Size())
	}

	if err := eng.Unmount(context.Background(), target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false
}

// TestSealTwiceDeterminismE2E proves sealing an already-sealed image through the
// full registry round-trip is a no-op: the second seal returns the first sealed
// digest as both its input and its output.
func TestSealTwiceDeterminismE2E(t *testing.T) {
	requireMkcomposefs(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay := buildCanonicalSealLayout(t, hostPlatform(), sealObjectBody)
	pushLayoutImage(t, registry, "registry.example.com/team/twice:input", lay)

	first, err := ociseal.Seal(context.Background(), "registry.example.com/team/twice@"+lay.Root.String(), ociseal.Options{Tag: "sealed"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first.Sealed == first.Original {
		t.Fatal("Seal did not change the root digest")
	}
	second, err := ociseal.Seal(context.Background(), "registry.example.com/team/twice@"+first.Sealed.String(), ociseal.Options{Tag: "resealed"})
	if err != nil {
		t.Fatalf("Seal second pass: %v", err)
	}
	if second.Sealed != first.Sealed || second.Original != first.Sealed {
		t.Fatalf("reseal result = %#v, want sealed and original %s", second, first.Sealed)
	}
}

// TestSealedPullRejectsTamperedAnnotationsE2E confirms the fresh-pull path fails
// closed at the integration level when a sealed canonical image reaches the
// device with a merged or config annotation that disagrees with its content.
func TestSealedPullRejectsTamperedAnnotationsE2E(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay := buildCanonicalSealLayout(t, hostPlatform(), sealObjectBody)
	pushLayoutImage(t, registry, "registry.example.com/team/tamper:input", lay)
	sealed, err := ociseal.Seal(context.Background(), "registry.example.com/team/tamper@"+lay.Root.String(), ociseal.Options{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealedRaw := fetchManifestRaw(t, registry, "registry.example.com/team/tamper@"+sealed.Sealed.String())

	wrong := strings.Repeat("0", 64)
	for _, tt := range []struct {
		name   string
		mutate func(*ocispec.Manifest)
	}{
		{"merged", func(m *ocispec.Manifest) { m.Annotations = map[string]string{ociseal.MergedAnnotation: wrong} }},
		{"config", func(m *ocispec.Manifest) { m.Config.Annotations = map[string]string{ociseal.ConfigAnnotation: wrong} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var manifest ocispec.Manifest
			if err := json.Unmarshal(sealedRaw, &manifest); err != nil {
				t.Fatalf("parse sealed manifest: %v", err)
			}
			tt.mutate(&manifest)
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("marshal tampered manifest: %v", err)
			}
			root := digest.FromBytes(raw)
			ref := "registry.example.com/team/tamper@" + root.String()
			pushRawManifest(t, registry, ref, raw, ocispec.MediaTypeImageManifest)

			eng, err := New(Config{Root: storeRoot})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = eng.Pull(context.Background(), ref)
			if devtest.IsVerityUnsupported(err) {
				t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), "annotation") {
				t.Fatalf("Pull error = %v, want an annotation mismatch", err)
			}
		})
	}
}

// TestSealFallbackE2E covers the --allow-non-canonical escape hatch end to
// end on an artifact-like tree (no /usr, so the canonical construction cannot
// apply): sealing records the deterministic fallback digest under the knarr
// fallback annotations, a device pull re-derives the same fallback digest and
// verifies it, and a tampered fallback annotation fails the pull closed. A
// fallback-sealed manifest rewritten to carry the standard merged annotation
// is rejected outright, since that namespace promises a canonical digest.
func TestSealFallbackE2E(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "model", Dir: true},
		octest.File{Name: "model/weights.bin", Mode: 0o644, Body: sealObjectBody},
		octest.File{Name: "model/config.json", Mode: 0o644, Body: "{}"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	pushLayoutImage(t, registry, "registry.example.com/team/model:input", lay)

	if _, err := ociseal.Seal(context.Background(), "registry.example.com/team/model@"+lay.Root.String(), ociseal.Options{}); !errors.Is(err, ociseal.ErrNotCanonical) {
		t.Fatalf("Seal without permission err = %v, want ErrNotCanonical", err)
	}
	sealed, err := ociseal.Seal(context.Background(), "registry.example.com/team/model@"+lay.Root.String(), ociseal.Options{AllowNonCanonical: true})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	ref := "registry.example.com/team/model@" + sealed.Sealed.String()
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.FsverityDigest != sealed.Platforms[0].MergedFsverity {
		t.Fatalf("device fs-verity = %s, publisher computed %s", info.FsverityDigest, sealed.Platforms[0].MergedFsverity)
	}

	sealedRaw := fetchManifestRaw(t, registry, ref)
	wrong := strings.Repeat("0", 64)
	for _, tt := range []struct {
		name    string
		mutate  func(*ocispec.Manifest)
		wantErr string
	}{
		{"tampered fallback", func(m *ocispec.Manifest) {
			m.Annotations = map[string]string{ociseal.FallbackMergedAnnotation: wrong}
		}, "annotation"},
		{"standard key on fallback build", func(m *ocispec.Manifest) {
			m.Annotations = map[string]string{ociseal.MergedAnnotation: m.Annotations[ociseal.FallbackMergedAnnotation]}
		}, "non-canonical"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var manifest ocispec.Manifest
			if err := json.Unmarshal(sealedRaw, &manifest); err != nil {
				t.Fatalf("parse sealed manifest: %v", err)
			}
			tt.mutate(&manifest)
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("marshal tampered manifest: %v", err)
			}
			root := digest.FromBytes(raw)
			tamperedRef := "registry.example.com/team/model@" + root.String()
			pushRawManifest(t, registry, tamperedRef, raw, ocispec.MediaTypeImageManifest)
			if _, err := eng.Pull(context.Background(), tamperedRef); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Pull error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestSealedCachedPullRejectsTamperAfterRestartE2E proves the cached fast path
// still fails closed after a restart: a second engine reading a previously
// cached image whose stored manifest carries a merged annotation disagreeing
// with the recorded content rejects it rather than returning a stale cache hit.
func TestSealedCachedPullRejectsTamperAfterRestartE2E(t *testing.T) {
	storeRoot := sealVerityStore(t)

	lay := buildCanonicalSealLayout(t, hostPlatform(), sealObjectBody)
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	imgStore, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	binding, err := imgStore.Get(lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("imagestore.Get: %v", err)
	}
	identity := rebindManifestAnnotations(t, obj, imgStore, lay, binding,
		map[string]string{ociseal.MergedAnnotation: strings.Repeat("0", 64)}, nil)

	// A fresh engine against the same on-disk store models a process restart
	// reading the cached image through the fast path, not a network pull.
	restarted, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	_, err = restarted.Pull(context.Background(), "registry.example.com/team/cached@"+identity.Digest.String(), WithFromDir(lay.Dir))
	if err == nil || !strings.Contains(err.Error(), "annotation") {
		t.Fatalf("cached Pull error = %v, want an annotation mismatch", err)
	}
}

// TestSealedIndexMultiArchPassthroughE2E seals a two-platform index carrying a
// buildx-style attestation descriptor, then pulls the host platform's sealed
// image and verifies it, confirms the attestation descriptor survived the seal
// untouched, and confirms the non-host platform's sealed manifest carries the
// annotation the seal computed for it.
func TestSealedIndexMultiArchPassthroughE2E(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	host := hostPlatform()
	foreign := ocispec.Platform{OS: "linux", Architecture: foreignArch(host.Architecture)}
	hostLay := buildCanonicalSealLayout(t, host, "host arch object payload beyond the sixty-four byte inline threshold\n")
	foreignLay := buildCanonicalSealLayout(t, foreign, "foreign arch object payload beyond the sixty-four byte inline threshold\n")
	hostImg := layoutToImage(t, hostLay)
	foreignImg := layoutToImage(t, foreignLay)
	pushLayoutImage(t, registry, "registry.example.com/team/multi:host", hostLay)
	pushLayoutImage(t, registry, "registry.example.com/team/multi:foreign", foreignLay)

	repo := "registry.example.com/team/multi"
	// The shape Docker Hub publishes: an attestation manifest carries a regular
	// image config and is distinguishable from an image only by its layer types.
	attRaw, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageManifest,
		"config": map[string]any{
			"mediaType": ocispec.MediaTypeImageConfig,
			"digest":    "sha256:" + strings.Repeat("c", 64),
			"size":      json.Number("233"),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.in-toto+json",
			"digest":    "sha256:" + strings.Repeat("d", 64),
			"size":      json.Number("1024"),
		}},
		"annotations": map[string]any{"vnd.docker.reference.type": "attestation-manifest"},
	})
	if err != nil {
		t.Fatalf("marshal attestation: %v", err)
	}
	attDigest := digest.FromBytes(attRaw)
	pushRawManifest(t, registry, repo+"@"+attDigest.String(), attRaw, ocispec.MediaTypeImageManifest)
	attDescriptor := map[string]any{
		"mediaType":   ocispec.MediaTypeImageManifest,
		"digest":      attDigest.String(),
		"size":        json.Number(fmt.Sprint(len(attRaw))),
		"platform":    map[string]any{"os": "unknown", "architecture": "unknown"},
		"annotations": map[string]any{"vnd.docker.reference.type": "attestation-manifest"},
	}
	indexRaw, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     ocispec.MediaTypeImageIndex,
		"manifests": []any{
			descriptorMap(imageDescriptor(t, hostImg, host)),
			descriptorMap(imageDescriptor(t, foreignImg, foreign)),
			attDescriptor,
		},
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	indexRoot := digest.FromBytes(indexRaw)
	pushRawManifest(t, registry, repo+"@"+indexRoot.String(), indexRaw, ocispec.MediaTypeImageIndex)

	// One platform per call: the host platform first, then the foreign one on
	// the index digest the first call produced.
	firstSeal, err := ociseal.Seal(context.Background(), repo+"@"+indexRoot.String(), ociseal.Options{Platform: "linux/" + host.Architecture})
	if err != nil {
		t.Fatalf("Seal host platform: %v", err)
	}
	if len(firstSeal.Platforms) != 1 {
		t.Fatalf("first seal platforms = %d, want 1", len(firstSeal.Platforms))
	}
	sealed, err := ociseal.Seal(context.Background(), repo+"@"+firstSeal.Sealed.String(), ociseal.Options{Platform: "linux/" + foreign.Architecture})
	if err != nil {
		t.Fatalf("Seal foreign platform: %v", err)
	}
	if len(sealed.Platforms) != 1 {
		t.Fatalf("second seal platforms = %d, want 1", len(sealed.Platforms))
	}

	ref := repo + "@" + sealed.Sealed.String()
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	info, err := eng.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	hostResult := platformResult(t, firstSeal, host.Architecture)
	if info.FsverityDigest != hostResult.MergedFsverity {
		t.Fatalf("device fs-verity = %s, publisher computed %s", info.FsverityDigest, hostResult.MergedFsverity)
	}

	// The buildx attestation descriptor is not an image and must ride through
	// the seal byte-for-byte.
	sealedIndexRaw := fetchManifestRaw(t, registry, ref)
	var sealedIndex ocispec.Index
	if err := json.Unmarshal(sealedIndexRaw, &sealedIndex); err != nil {
		t.Fatalf("parse sealed index: %v", err)
	}
	var sawAttestation bool
	for _, m := range sealedIndex.Manifests {
		if m.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			sawAttestation = true
			if m.Digest != attDigest {
				t.Fatalf("attestation descriptor digest = %s, want %s", m.Digest, attDigest)
			}
		}
	}
	if !sawAttestation {
		t.Fatal("sealed index dropped the attestation descriptor")
	}

	// The foreign platform is not pullable on this host, but its sealed manifest
	// must carry the merged annotation the seal computed for it.
	foreignResult := platformResult(t, sealed, foreign.Architecture)
	foreignAnn := publishedMergedAnnotation(t, registry, repo+"@"+foreignResult.Sealed.String())
	if want := strings.TrimPrefix(foreignResult.MergedFsverity, "sha256:"); foreignAnn != want {
		t.Fatalf("foreign merged annotation = %s, want %s", foreignAnn, want)
	}
}

// TestSealDeclinesNonCanonicalE2E confirms sealing a non-canonical image
// declines with ErrNotCanonical and writes nothing to the registry.
func TestSealDeclinesNonCanonicalE2E(t *testing.T) {
	requireMkcomposefs(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	// A tree without /usr diverges from the canonical construction.
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/value", Mode: 0o644, Body: "value"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	pushLayoutImage(t, registry, "registry.example.com/team/decline:input", lay)

	_, err = ociseal.Seal(context.Background(), "registry.example.com/team/decline@"+lay.Root.String(), ociseal.Options{Tag: "sealed"})
	if !errors.Is(err, ociseal.ErrNotCanonical) {
		t.Fatalf("Seal err = %v, want ErrNotCanonical", err)
	}
	tag, err := name.NewTag("registry.example.com/team/decline:sealed")
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if _, err := remote.Get(tag, remote.WithTransport(registry.Transport())); err == nil {
		t.Fatal("declined seal tagged the image")
	}
}

// TestSealedNonCanonicalPullFailsClosedE2E confirms the fresh-pull path rejects
// a non-canonical image that carries a merged-EROFS annotation, since such an
// image cannot reproduce the value the annotation claims.
func TestSealedNonCanonicalPullFailsClosedE2E(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	// A tree without /usr diverges from the canonical construction; the object
	// payload is stored by digest so the pull builds a real merged image.
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config", Mode: 0o644, Body: sealObjectBody},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	pushLayoutImage(t, registry, "registry.example.com/team/uncanon:input", lay)

	raw := fetchManifestRaw(t, registry, "registry.example.com/team/uncanon@"+lay.Root.String())
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	manifest.Annotations = map[string]string{ociseal.MergedAnnotation: strings.Repeat("a", 64)}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal sealed manifest: %v", err)
	}
	root := digest.FromBytes(raw)
	ref := "registry.example.com/team/uncanon@" + root.String()
	pushRawManifest(t, registry, ref, raw, ocispec.MediaTypeImageManifest)

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = eng.Pull(context.Background(), ref)
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("Pull error = %v, want a non-canonical rejection", err)
	}
}

// sealVerityStore returns a fresh verity-capable store root, skipping when
// fs-verity or mkcomposefs is unavailable.
func sealVerityStore(t *testing.T) string {
	t.Helper()
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	requireMkcomposefs(t)
	return storeRoot
}

// requireMkcomposefs skips the test unless mkcomposefs is on PATH.
func requireMkcomposefs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
}

// buildCanonicalSealLayout writes a canonical single-layer image whose tree
// carries a >64-byte object file, an inline file, and an empty file.
func buildCanonicalSealLayout(t *testing.T, platform ocispec.Platform, objectBody string) octest.Layout {
	t.Helper()
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "usr", Dir: true},
		octest.File{Name: "usr/bin", Dir: true},
		octest.File{Name: "usr/bin/tool", Mode: 0o755, Body: objectBody},
		octest.File{Name: "etc", Dir: true},
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "sealed image\n"},
		octest.File{Name: "empty", Mode: 0o644},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	return lay
}

// layoutToImage opens an OCI layout directory and returns its pinned image.
func layoutToImage(t *testing.T, lay octest.Layout) ggcrv1.Image {
	t.Helper()
	path, err := ggcrlayout.FromPath(lay.Dir)
	if err != nil {
		t.Fatalf("layout.FromPath: %v", err)
	}
	hash, err := ggcrv1.NewHash(lay.Root.String())
	if err != nil {
		t.Fatalf("v1.NewHash: %v", err)
	}
	image, err := path.Image(hash)
	if err != nil {
		t.Fatalf("layout.Image: %v", err)
	}
	return image
}

// pushLayoutImage uploads lay's image to registry under ref.
func pushLayoutImage(t *testing.T, registry *octest.Registry, ref string, lay octest.Layout) {
	t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatalf("name.NewTag: %v", err)
	}
	if err := remote.Write(tag, layoutToImage(t, lay), remote.WithTransport(registry.Transport())); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
}

// imageDescriptor returns a platform-qualified descriptor pinning image.
func imageDescriptor(t *testing.T, image ggcrv1.Image, platform ocispec.Platform) ocispec.Descriptor {
	t.Helper()
	hash, err := image.Digest()
	if err != nil {
		t.Fatalf("image.Digest: %v", err)
	}
	raw, err := image.RawManifest()
	if err != nil {
		t.Fatalf("image.RawManifest: %v", err)
	}
	mediaType, err := image.MediaType()
	if err != nil {
		t.Fatalf("image.MediaType: %v", err)
	}
	return ocispec.Descriptor{
		MediaType: string(mediaType),
		Digest:    digest.Digest(hash.String()),
		Size:      int64(len(raw)),
		Platform:  &platform,
	}
}

// descriptorMap renders desc as the JSON descriptor map an index entry carries.
func descriptorMap(desc ocispec.Descriptor) map[string]any {
	doc := map[string]any{
		"mediaType": desc.MediaType,
		"digest":    desc.Digest.String(),
		"size":      json.Number(fmt.Sprint(desc.Size)),
		"platform": map[string]any{
			"os":           desc.Platform.OS,
			"architecture": desc.Platform.Architecture,
		},
	}
	if desc.Platform.Variant != "" {
		doc["platform"].(map[string]any)["variant"] = desc.Platform.Variant
	}
	return doc
}

// pushRawManifest publishes raw under ref through the registry manifest endpoint.
func pushRawManifest(t *testing.T, registry *octest.Registry, ref string, raw []byte, mediaType string) {
	t.Helper()
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		t.Fatalf("name.NewDigest: %v", err)
	}
	taggable := rawTaggable{data: raw, mediaType: types.MediaType(mediaType)}
	if err := remote.Put(digestRef, taggable, remote.WithTransport(registry.Transport())); err != nil {
		t.Fatalf("remote.Put: %v", err)
	}
}

// fetchManifestRaw returns the raw manifest bytes the registry holds for ref.
func fetchManifestRaw(t *testing.T, registry *octest.Registry, ref string) []byte {
	t.Helper()
	digestRef, err := name.NewDigest(ref)
	if err != nil {
		t.Fatalf("name.NewDigest: %v", err)
	}
	desc, err := remote.Get(digestRef, remote.WithTransport(registry.Transport()))
	if err != nil {
		t.Fatalf("remote.Get: %v", err)
	}
	return desc.Manifest
}

// publishedMergedAnnotation reads the merged-EROFS annotation the registry holds
// for ref and returns its bare-hex value.
func publishedMergedAnnotation(t *testing.T, registry *octest.Registry, ref string) string {
	t.Helper()
	var manifest ocispec.Manifest
	if err := json.Unmarshal(fetchManifestRaw(t, registry, ref), &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	value, ok := manifest.Annotations[ociseal.MergedAnnotation]
	if !ok {
		t.Fatalf("manifest %s carries no merged annotation", ref)
	}
	return value
}

// platformResult returns the sealed platform result for the given architecture.
func platformResult(t *testing.T, result ociseal.Result, architecture string) ociseal.PlatformResult {
	t.Helper()
	for _, p := range result.Platforms {
		if p.Platform.Architecture == architecture {
			return p
		}
	}
	t.Fatalf("no sealed platform for architecture %q", architecture)
	return ociseal.PlatformResult{}
}

// foreignArch returns an architecture that never equals host, so a two-platform
// index has exactly one host-pullable entry.
func foreignArch(host string) string {
	if host == "amd64" {
		return "arm64"
	}
	return "amd64"
}

// rawTaggable publishes fixed manifest bytes under a chosen media type.
type rawTaggable struct {
	data      []byte
	mediaType types.MediaType
}

func (m rawTaggable) RawManifest() ([]byte, error) {
	return m.data, nil
}

func (m rawTaggable) MediaType() (types.MediaType, error) {
	return m.mediaType, nil
}
