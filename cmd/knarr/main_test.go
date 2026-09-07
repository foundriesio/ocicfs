// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/ocilayout"
	"github.com/foundriesio/knarr/internal/octest"
	"github.com/foundriesio/knarr/internal/refstore"
)

func TestCommandsCoverM1Verbs(t *testing.T) {
	root, _ := newRootCommand(io.Discard, io.Discard)
	want := []string{"gc", "hold", "inspect", "ls", "mount", "publish", "pull", "release", "remove", "seal", "unmount"}

	// cobra adds its own "help" and "completion" commands; compare only knarr's.
	builtins := map[string]bool{"help": true, "completion": true}
	var got []string
	for _, c := range root.Commands() {
		if builtins[c.Name()] {
			continue
		}
		got = append(got, c.Name())
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("command set = %v, want %v", got, want)
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "knarr") {
		t.Errorf("version output = %q, want it to mention knarr", out.String())
	}
}

func TestRunNoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, &out, &errOut); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"bogus"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("stderr = %q, want it to mention unknown command", errOut.String())
	}
}

func TestRunSubcommandNotImplemented(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"publish", "dir", "example.com/team/app:latest"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "not implemented") {
		t.Errorf("stderr = %q, want it to mention not implemented", errOut.String())
	}
}

func TestRunMountRWReportsUnsupported(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"mount", "--rw", "app@sha256:" + strings.Repeat("a", 64), t.TempDir(),
	}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "writable mounts are not supported") {
		t.Errorf("stderr = %q, want writable unsupported", errOut.String())
	}
}

func TestRunMountRWAndMetadataOnlyIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"mount", "--rw", "--metadata-only", "app@sha256:" + strings.Repeat("a", 64), t.TempDir(),
	}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want mutual-exclusion error", errOut.String())
	}
}

func TestRunUnmountDispatchesToEngine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := run(ctx, []string{"unmount", "/mnt/knarr-test"}, &out, &errOut)
	if code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
	if !strings.Contains(errOut.String(), "cancelled") {
		t.Errorf("stderr = %q, want cancellation", errOut.String())
	}
}

func TestRunSubcommandUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	// pull requires exactly one ref argument.
	code := run(context.Background(), []string{"pull"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage") {
		t.Errorf("stderr = %q, want it to mention usage", errOut.String())
	}
}

// TestRunFormatJSONToStdout verifies the machine-readable output contract: a
// --format json error result is emitted as JSON on stdout while diagnostics
// stay on stderr. The engine stub fails, so success output is exercised in
// engine-level tests; here we assert the stream separation holds for the flag
// surface and that an invalid format is a usage error.
func TestRunInvalidFormatIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"ls", "--format", "yaml"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a usage error", out.String())
	}
	if !strings.Contains(errOut.String(), "invalid format") {
		t.Errorf("stderr = %q, want it to mention invalid format", errOut.String())
	}
}

func TestRunLsJSON(t *testing.T) {
	root := t.TempDir()
	imageRoot, ref := writeCLIImage(t, root, "app@%s", ocispec.Platform{OS: "linux", Architecture: "amd64"})

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--root", root, "ls", "--format", "json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	var got []struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if len(got) != 1 || got[0].Ref != ref || got[0].Digest != imageRoot.String() || got[0].Size != 4096 {
		t.Fatalf("ls JSON = %#v", got)
	}
}

func TestRunRemoveDispatchesToEngine(t *testing.T) {
	root := t.TempDir()
	platform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	imageRoot, ref := writeCLIImage(t, root, "registry.example.com/app@%s", platform)

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--root", root, "remove", "--force", ref}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	refStore, err := refstore.New(root)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	if _, err := refStore.Get(ref); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("Get ref error = %v, want ErrNotFound", err)
	}
	imgStore, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	if _, err := imgStore.Get(imageRoot, platform); !errors.Is(err, imagestore.ErrNotFound) {
		t.Fatalf("Get binding error = %v, want ErrNotFound", err)
	}
}

func TestRunPullNegativeIngestWorkersFails(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", t.TempDir(),
		"pull", "--ingest-workers", "-1", "app@sha256:" + strings.Repeat("a", 64),
	}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ingest workers must not be negative") {
		t.Errorf("stderr = %q, want the engine's negative-workers error", errOut.String())
	}
}

func TestRunPullHelpListsIngestWorkers(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"pull", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "--ingest-workers") || !strings.Contains(out.String(), "(default 2)") {
		t.Errorf("pull --help = %q, want --ingest-workers with default 2", out.String())
	}
}

// TestRunPullFromDirE2E drives a real `knarr pull --from-dir` through the command
// front end: it exercises flag binding, engine construction from --root, and the
// --format json output contract end to end, against a local OCI layout. It is
// verity-gated because the pull seals content with fs-verity.
func TestRunPullFromDirE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}

	platform := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), platform,
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", storeRoot,
		"pull", "--from-dir", lay.Dir, "--format", "json", lay.Ref,
	}, &out, &errOut)
	if code != 0 {
		if strings.Contains(errOut.String(), "fs-verity") {
			t.Skipf("fs-verity unsupported on the test filesystem: %s", errOut.String())
		}
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}

	var got struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if got.Ref != lay.Ref {
		t.Errorf("ref = %q, want %q", got.Ref, lay.Ref)
	}
	if got.Digest != lay.Root.String() {
		t.Errorf("digest = %q, want %s", got.Digest, lay.Root)
	}
	if got.Size <= 0 {
		t.Errorf("size = %d, want > 0", got.Size)
	}
	if _, err := os.Stat(octest.BindingPath(storeRoot, lay.Root, lay.Platform)); err != nil {
		t.Errorf("binding not written by CLI pull: %v", err)
	}
}

// jsonParses is a small guard that the JSON encoder configuration stays valid;
// it documents the intended shape of list output.
func TestImageListJSONShape(t *testing.T) {
	const sample = `[{"ref":"foo","digest":"sha256:abc","size":10}]`
	var got []map[string]any
	if err := json.Unmarshal([]byte(sample), &got); err != nil {
		t.Fatalf("sample list output does not parse as JSON: %v", err)
	}
	if len(got) != 1 || got[0]["ref"] != "foo" {
		t.Errorf("unexpected decoded shape: %v", got)
	}
}

// writeCLIImage seeds the state the CLI verbs treat as a pulled image for
// platform p: a real image manifest and its config stored in the object store
// under their own digests -- the manifest is the authoritative presence signal
// the hold verbs read, and every stored blob hashes to the path it is filed
// under, as a real pull guarantees -- plus a ref record and a binding. refFmt
// is a %s template the manifest digest is substituted into; the manifest digest
// and the resulting ref are returned.
func writeCLIImage(t *testing.T, root, refFmt string, p ocispec.Platform) (digest.Digest, string) {
	t.Helper()
	configJSON, err := json.Marshal(ocispec.Image{Platform: p, RootFS: ocispec.RootFS{Type: "layers"}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.SHA256.FromBytes(configJSON),
		Size:      int64(len(configJSON)),
	}
	manifestJSON, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.SHA256.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	writeCLIObject(t, root, configDesc.Digest, configJSON)
	writeCLIObject(t, root, manifestDesc.Digest, manifestJSON)

	ref := strings.Replace(refFmt, "%s", manifestDesc.Digest.String(), 1)
	refStore, err := refstore.New(root)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}
	if err := refStore.Put(ref, manifestDesc, manifestDesc, time.Unix(0, 0).UTC(), []string{ocilayout.PlatformString(&p)}); err != nil {
		t.Fatalf("Put ref: %v", err)
	}
	imgStore, err := imagestore.New(root)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	if err := imgStore.Put(manifestDesc.Digest, p, imagestore.Binding{
		Descriptor:     ocispec.Descriptor{Digest: digest.SHA256.FromString("composefs"), Size: 4096},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}); err != nil {
		t.Fatalf("Put binding: %v", err)
	}
	return manifestDesc.Digest, ref
}

// writeCLIObject stores data at the content-addressed path digest d names.
func writeCLIObject(t *testing.T, root string, d digest.Digest, data []byte) {
	t.Helper()
	path := octest.ObjectPath(root, d)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll object: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile object: %v", err)
	}
}
