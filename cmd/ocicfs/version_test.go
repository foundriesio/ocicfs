// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

func TestRunVersionCommand(t *testing.T) {
	root := t.TempDir()
	eng, err := ocicfs.New(ocicfs.Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state, err := eng.VerityState()
	if err != nil {
		t.Fatalf("VerityState: %v", err)
	}
	if state != "enabled" && state != "disabled" && state != "unsupported" {
		t.Fatalf("VerityState = %q", state)
	}

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--root", root, "version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := "verity: " + state + " (build: " + ocicfs.BuildVerityMode + ")"
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ocicfs ") || lines[1] != want {
		t.Fatalf("version output = %q, want the version line, then %q", out.String(), want)
	}

	out.Reset()
	if code := run(context.Background(), []string{"--root", root, "version", "--format", "json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
	}
	var got cli.VersionOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if got.Verity.Build != ocicfs.BuildVerityMode || got.Verity.State != state {
		t.Fatalf("version JSON = %s", out.String())
	}
}

// TestWriteVersionUnknownState pins the output for a store whose fs-verity
// state could not be determined: the text names it unknown and the JSON
// leaves it out.
func TestWriteVersionUnknownState(t *testing.T) {
	var text bytes.Buffer
	if err := cli.WriteVersion(&text, cli.FormatText, "v1", "abc", ""); err != nil {
		t.Fatalf("WriteVersion: %v", err)
	}
	want := "ocicfs v1 (abc)\nverity: unknown (build: " + ocicfs.BuildVerityMode + ")\n"
	if text.String() != want {
		t.Fatalf("text = %q, want %q", text.String(), want)
	}
	var buf bytes.Buffer
	if err := cli.WriteVersion(&buf, cli.FormatJSON, "v1", "abc", ""); err != nil {
		t.Fatalf("WriteVersion: %v", err)
	}
	var got struct {
		Verity map[string]string `json:"verity"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, buf.String())
	}
	if _, has := got.Verity["state"]; has || got.Verity["build"] != ocicfs.BuildVerityMode {
		t.Fatalf("json verity = %v, want the build mode alone", got.Verity)
	}
}

func TestWriteImageInfoVerityRow(t *testing.T) {
	info := &ocicfs.ImageInfo{}
	build := ocicfs.BuildVerityMode
	var text bytes.Buffer
	if err := cli.WriteImageInfo(&text, cli.FormatText, info, "disabled", false); err != nil {
		t.Fatalf("WriteImageInfo: %v", err)
	}
	if !strings.Contains(text.String(), "verity     disabled (build: "+build+")\n") {
		t.Errorf("text output lacks the verity row:\n%s", text.String())
	}
	var buf bytes.Buffer
	if err := cli.WriteImageInfo(&buf, cli.FormatJSON, info, "unsupported", false); err != nil {
		t.Fatalf("WriteImageInfo: %v", err)
	}
	var got cli.ImageInfoOutput
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Verity.Build != build || got.Verity.State != "unsupported" {
		t.Fatalf("json verity = %+v", got.Verity)
	}
	text.Reset()
	if err := cli.WriteImageInfo(&text, cli.FormatText, info, "", false); err != nil {
		t.Fatalf("WriteImageInfo: %v", err)
	}
	if !strings.Contains(text.String(), "verity     unknown (build: "+build+")\n") {
		t.Errorf("text output lacks the unknown verity row:\n%s", text.String())
	}
}
