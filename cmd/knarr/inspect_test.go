// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/pkg/knarr"
)

func TestRunInspectArgValidation(t *testing.T) {
	for _, args := range [][]string{{"inspect"}, {"inspect", "a", "b"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) exit = %d, want 2 (usage)", args, code)
		}
	}
}

func TestRunInspectUnknownImage(t *testing.T) {
	root := t.TempDir()
	ref := digest.SHA256.FromString("absent").String()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--root", root, "inspect", ref}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "knarr pull") {
		t.Errorf("stderr = %q, want it to suggest knarr pull", errOut.String())
	}
}

func TestWriteImageInfoRendering(t *testing.T) {
	info := &knarr.ImageInfo{
		Root:     digest.SHA256.FromString("root"),
		Platform: ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		DiffIDs:  []digest.Digest{digest.SHA256.FromString("l0"), digest.SHA256.FromString("l1")},
		Composefs: ocispec.Descriptor{
			MediaType: "application/vnd.knarr.composefs.image.v1+erofs",
			Digest:    digest.SHA256.FromString("cfs"),
			Size:      4096,
		},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}

	var text bytes.Buffer
	if err := cli.WriteImageInfo(&text, cli.FormatText, info); err != nil {
		t.Fatalf("WriteImageInfo text: %v", err)
	}
	for _, want := range []string{
		"root\t" + info.Root.String(),
		"platform\tlinux/arm64/v8",
		"diff_id\t" + info.DiffIDs[0].String(),
		"diff_id\t" + info.DiffIDs[1].String(),
		"composefs\t" + info.Composefs.Digest.String(),
		"fsverity\t" + info.FsverityDigest,
	} {
		if !strings.Contains(text.String(), want+"\n") {
			t.Errorf("text output missing %q\ngot:\n%s", want, text.String())
		}
	}

	var jsonBuf bytes.Buffer
	if err := cli.WriteImageInfo(&jsonBuf, cli.FormatJSON, info); err != nil {
		t.Fatalf("WriteImageInfo json: %v", err)
	}
	var got cli.ImageInfoOutput
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if got.Root != info.Root.String() || got.Platform != "linux/arm64/v8" || len(got.DiffIDs) != 2 ||
		got.Composefs.Digest != info.Composefs.Digest.String() || got.Fsverity != info.FsverityDigest {
		t.Fatalf("json output = %#v", got)
	}
}
