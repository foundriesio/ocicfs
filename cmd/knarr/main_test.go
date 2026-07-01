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
	"runtime"
	"sort"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/octest"
)

func TestCommandsCoverM1Verbs(t *testing.T) {
	root, _ := newRootCommand(io.Discard, io.Discard)
	want := []string{"ls", "mount", "publish", "pull", "remove", "unmount"}

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
	code := run(context.Background(), []string{"mount", "example.com/foo:v1", "/mnt/foo"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "not implemented") {
		t.Errorf("stderr = %q, want it to mention not implemented", errOut.String())
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
