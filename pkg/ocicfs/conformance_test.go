// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/imagestore"
	"github.com/foundriesio/ocicfs/internal/octest"
)

// The conformance oracle is a cfsctl repository initialised with ocicfs's
// fs-verity algorithm and EROFS format, so the merged image it builds matches
// the one ocicfs must reproduce. cfsctl's init default is fsverity-sha512 (EROFS
// V1 is its init default), so the algorithm is set explicitly and the repository
// metadata cfsctl wrote is asserted before any comparison, so a drifted default
// cannot silently change what the oracle computes.
const (
	oracleAlgorithm    = "fsverity-sha256-12"
	oracleErofsVersion = 1 // EROFS format V1, C mkcomposefs 1.0.8-compatible
	oracleTag          = "ocicfs-oracle"
)

// TestConformanceCrossToolEqualityE2E proves ocicfs builds the canonical merged
// EROFS: for each image ocicfs constructs canonically, its kernel-measured merged
// fs-verity digest must equal the digest cfsctl computes from the same OCI layers
// with `oci compute-id`. This is the release gate for sealing — a digest sealed
// under the shared annotation key must be one the reference tooling reproduces.
// It needs the privileged dev container (fs-verity) with cfsctl and composefs-info
// on PATH, and skips otherwise.
func TestConformanceCrossToolEqualityE2E(t *testing.T) {
	requireOracleTools(t)

	for _, tc := range []struct {
		name  string
		files []octest.File
	}{
		{
			// A standard image with a populated /usr: the canonical construction
			// takes the root directory's metadata from /usr. The large file is
			// stored by digest, the small one is inlined.
			name: "standard-usr",
			files: []octest.File{
				{Name: "usr", Dir: true},
				{Name: "usr/bin", Dir: true},
				{Name: "usr/bin/tool", Mode: 0o755, Body: strings.Repeat("x", 200)},
				{Name: "etc", Dir: true},
				{Name: "etc/hostname", Mode: 0o644, Body: "host\n"},
			},
		},
		{
			// The inline-threshold boundary: 0 and 1 and 64-byte files are inlined
			// into the EROFS, the 65-byte file is stored by digest. ocicfs and cfsctl
			// must agree on the 64-byte cutoff for the images to match.
			name: "inline-boundary",
			files: []octest.File{
				{Name: "usr", Dir: true},
				{Name: "usr/share", Dir: true},
				{Name: "usr/share/s0", Mode: 0o644, Body: ""},
				{Name: "usr/share/s1", Mode: 0o644, Body: "a"},
				{Name: "usr/share/s64", Mode: 0o644, Body: strings.Repeat("a", 64)},
				{Name: "usr/share/s65", Mode: 0o644, Body: strings.Repeat("a", 65)},
			},
		},
		{
			// A populated /run: the canonical construction empties it in place,
			// keeping the directory node with the metadata source's mtime. The
			// image-supplied file must be gone from both merged images.
			name: "run-emptied",
			files: []octest.File{
				{Name: "usr", Dir: true},
				{Name: "usr/lib", Dir: true},
				{Name: "usr/lib/data", Mode: 0o644, Body: strings.Repeat("d", 128)},
				{Name: "run", Dir: true},
				{Name: "run/leftover", Mode: 0o644, Body: "transient\n"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lay, storeRoot, binding := pullFixture(t, tc.files)
			if !binding.Canonical {
				t.Fatalf("fixture %q was built non-canonically (binding canonical=false); it is not eligible for the cross-tool gate", tc.name)
			}

			cfsRepo := initOracleRepo(t, storeRoot)
			runCfsctl(t, "--repo", cfsRepo, "oci", "pull", "oci:"+lay.Dir, oracleTag)
			oracle := strings.TrimSpace(runCfsctl(t, "--repo", cfsRepo, "oci", "compute-id", oracleTag))

			ocicfsHex := strings.TrimPrefix(binding.FsverityDigest, "sha256:")
			if ocicfsHex == oracle {
				return
			}

			// Byte divergence: dump both merged images so the difference is visible.
			erofs := octest.ObjectPath(storeRoot, binding.Descriptor.Digest)
			t.Errorf("merged fs-verity digest mismatch: ocicfs %s, cfsctl %s", ocicfsHex, oracle)
			t.Errorf("ocicfs merged EROFS (composefs-info dump):\n%s", runTool(t, "composefs-info", "dump", erofs))
			t.Errorf("cfsctl merged EROFS (oci dump):\n%s", runCfsctl(t, "--repo", cfsRepo, "oci", "dump", oracleTag))
		})
	}
}

// TestConformanceFallbackDeterminismE2E covers images the canonical construction
// cannot reproduce (no /usr directory, a non-directory /usr or /run, layers
// relying on synthesized parents): ocicfs builds them with its deterministic
// fallback and marks them non-canonical. The reference tooling cannot process
// them, so there is no cross-tool digest to compare; instead each is built twice
// in independent stores and must yield the same merged digest, and the binding
// must record canonical=false so sealing later declines it. Needs the privileged
// dev container (fs-verity); skips otherwise.
func TestConformanceFallbackDeterminismE2E(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	for _, tc := range []struct {
		name  string
		files []octest.File
	}{
		{
			// No /usr: the root metadata falls back to the canonical default.
			name: "no-usr",
			files: []octest.File{
				{Name: "bin", Dir: true},
				{Name: "bin/sh", Mode: 0o755, Body: strings.Repeat("s", 200)},
				{Name: "etc", Dir: true},
				{Name: "etc/hostname", Mode: 0o644, Body: "host\n"},
			},
		},
		{
			// /usr present but not a directory: the root metadata has no /usr to
			// come from, so ocicfs falls back.
			name: "usr-not-dir",
			files: []octest.File{
				{Name: "usr", Mode: 0o644, Body: "not a directory"},
				{Name: "bin", Dir: true},
				{Name: "bin/sh", Mode: 0o755, Body: strings.Repeat("s", 200)},
			},
		},
		{
			// /run present but not a directory: it has no canonical empty form, so
			// ocicfs leaves it and falls back.
			name: "run-not-dir",
			files: []octest.File{
				{Name: "usr", Dir: true},
				{Name: "usr/bin", Dir: true},
				{Name: "usr/bin/tool", Mode: 0o755, Body: strings.Repeat("x", 200)},
				{Name: "run", Mode: 0o644, Body: "not a directory"},
			},
		},
		{
			// A file whose parent directories are never declared in the layer:
			// ocicfs synthesizes them, which the canonical construction does not.
			name: "implicit-parent",
			files: []octest.File{
				{Name: "usr", Dir: true},
				{Name: "opt/app/data", Mode: 0o644, Body: strings.Repeat("p", 200)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, first := pullFixture(t, tc.files)
			_, _, second := pullFixture(t, tc.files)

			if first.Canonical || second.Canonical {
				t.Fatalf("fixture %q was built canonically (canonical=true); it must use the non-canonical fallback", tc.name)
			}
			if first.FsverityDigest != second.FsverityDigest {
				t.Fatalf("fallback construction is not deterministic: %s then %s", first.FsverityDigest, second.FsverityDigest)
			}
		})
	}
}

// pullFixture writes the files as a single-layer OCI image, pulls it into a fresh
// verity-capable store via --from-dir, and returns the layout, the store root,
// and the recorded binding. It skips the test when the store filesystem cannot
// support fs-verity.
func pullFixture(t *testing.T, files []octest.File) (octest.Layout, string, imagestore.Binding) {
	t.Helper()

	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(), files...)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(t.Context(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	img, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	binding, err := img.Get(lay.Root, hostPlatform())
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	return lay, storeRoot, binding
}

// initOracleRepo creates a cfsctl repository beside the ocicfs store, pinned to
// ocicfs's fs-verity algorithm and EROFS format, and asserts the
// metadata cfsctl wrote so a drifted init default cannot silently change the
// oracle. It shares the store's verity-capable filesystem, which cfsctl's
// default (fs-verity-enabled) repository requires.
func initOracleRepo(t *testing.T, storeRoot string) string {
	t.Helper()

	repo := filepath.Join(storeRoot, "cfs-oracle")
	runCfsctl(t, "init", "--algorithm", oracleAlgorithm, repo)

	data, err := os.ReadFile(filepath.Join(repo, "meta.json")) // #nosec G304 -- test-owned repo path
	if err != nil {
		t.Fatalf("read oracle repo metadata: %v", err)
	}
	var meta struct {
		Algorithm    string `json:"algorithm"`
		ErofsFormats struct {
			Default int `json:"default"`
		} `json:"erofs_formats"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode oracle repo metadata: %v", err)
	}
	if meta.Algorithm != oracleAlgorithm {
		t.Fatalf("oracle repo algorithm is %q, want %q", meta.Algorithm, oracleAlgorithm)
	}
	if meta.ErofsFormats.Default != oracleErofsVersion {
		t.Fatalf("oracle repo EROFS version is %d, want %d", meta.ErofsFormats.Default, oracleErofsVersion)
	}
	return repo
}

// requireOracleTools skips the test unless the cross-tool oracle and the store's
// fs-verity backing are available.
func requireOracleTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"cfsctl", "composefs-info", "mkcomposefs"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found in PATH", bin)
		}
	}
}

func runCfsctl(t *testing.T, args ...string) string {
	t.Helper()
	return runTool(t, "cfsctl", args...)
}

func runTool(t *testing.T, name string, args ...string) string {
	t.Helper()
	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(t.Context(), name, args...) // #nosec G204 -- test helper, PATH-resolved binary and fixed args
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v (stderr=%s)", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String()
}
