// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/ocicfs/internal/cli"
	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/octest"
	"github.com/foundriesio/ocicfs/pkg/ocicfs"
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
	if !strings.Contains(errOut.String(), "ocicfs pull") {
		t.Errorf("stderr = %q, want it to suggest ocicfs pull", errOut.String())
	}
}

func TestWriteImageInfoRendering(t *testing.T) {
	info := &ocicfs.ImageInfo{
		Root:     digest.SHA256.FromString("root"),
		Platform: ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		DiffIDs:  []digest.Digest{digest.SHA256.FromString("l0"), digest.SHA256.FromString("l1")},
		Composefs: ocispec.Descriptor{
			MediaType: "application/vnd.ocicfs.composefs.image.v1+erofs",
			Digest:    digest.SHA256.FromString("cfs"),
			Size:      4096,
		},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
	}

	var text bytes.Buffer
	if err := cli.WriteImageInfo(&text, cli.FormatText, info, false); err != nil {
		t.Fatalf("WriteImageInfo text: %v", err)
	}
	lines := strings.Split(strings.TrimRight(text.String(), "\n"), "\n")
	wantRows := [][2]string{
		{"root", info.Root.String()},
		{"platform", "linux/arm64/v8"},
		{"composefs", info.Composefs.Digest.String()},
		{"fsverity", info.FsverityDigest},
	}
	if len(lines) != len(wantRows) {
		t.Fatalf("text output has %d lines, want %d:\n%s", len(lines), len(wantRows), text.String())
	}
	// Each row is "<key><spaces><value>" with the value column aligned across
	// all rows.
	valueCol := -1
	for i, want := range wantRows {
		line := lines[i]
		if !strings.HasPrefix(line, want[0]+" ") {
			t.Errorf("line %d = %q, want key %q", i, line, want[0])
			continue
		}
		rest := line[len(want[0]):]
		col := len(want[0]) + (len(rest) - len(strings.TrimLeft(rest, " ")))
		if valueCol == -1 {
			valueCol = col
		} else if col != valueCol {
			t.Errorf("line %d value starts at column %d, want the aligned column %d:\n%s", i, col, valueCol, text.String())
		}
		if got := line[col:]; got != want[1] {
			t.Errorf("line %d value = %q, want %q", i, got, want[1])
		}
	}
	// Per-layer detail, including the diff_ids, is --layers territory; the
	// default text output carries only the image identity and binding, and an
	// unknown (zero) rootfs size renders no row at all.
	if strings.Contains(text.String(), "diff_id") {
		t.Errorf("text output without --layers carries diff_id rows:\n%s", text.String())
	}
	if strings.Contains(text.String(), "rootfs") {
		t.Errorf("text output shows a rootfs row for an unknown size:\n%s", text.String())
	}

	var jsonBuf bytes.Buffer
	if err := cli.WriteImageInfo(&jsonBuf, cli.FormatJSON, info, false); err != nil {
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
	// Without --layers, and with no recorded rootfs size, the JSON contract is
	// byte-compatible with the pre-layer shape: exactly the five legacy keys —
	// no "layers", no "rootfs_size".
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(jsonBuf.Bytes(), &keys); err != nil {
		t.Fatalf("unmarshal json keys: %v", err)
	}
	for _, k := range []string{"root", "platform", "diff_ids", "composefs", "fsverity"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("json missing legacy key %q:\n%s", k, jsonBuf.String())
		}
	}
	if len(keys) != 5 {
		t.Errorf("json has %d keys, want exactly the 5 legacy fields:\n%s", len(keys), jsonBuf.String())
	}
}

func TestWriteImageInfoLayersRendering(t *testing.T) {
	layer0Digest := digest.SHA256.FromString("blob0")
	info := &ocicfs.ImageInfo{
		Root:     digest.SHA256.FromString("root"),
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		DiffIDs:  []digest.Digest{digest.SHA256.FromString("l0"), digest.SHA256.FromString("l1")},
		Composefs: ocispec.Descriptor{
			MediaType: "application/vnd.ocicfs.composefs.image.v1+erofs",
			Digest:    digest.SHA256.FromString("cfs"),
			Size:      4096,
		},
		FsverityDigest: digest.SHA256.FromString("verity").String(),
		RootfsSize:     25000000,
		Layers: []ocicfs.LayerInfo{
			{
				MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
				Digest:    layer0Digest,
				Size:      3565158,
				DiffID:    digest.SHA256.FromString("l0"),
				ChainID:   digest.SHA256.FromString("l0"),
				CreatedBy: "ADD rootfs.tar /",
			},
			{
				MediaType: "evil/\x1b[31m\u009btype+zstd",
				Digest:    digest.SHA256.FromString("blob1"),
				Size:      512,
				DiffID:    digest.SHA256.FromString("l1"),
				ChainID:   digest.SHA256.FromString("c1"),
				CreatedBy: "RUN \x1b[31mevil\x1b[0m\u009b31mc1\nforged\trow",
			},
		},
	}

	var text bytes.Buffer
	if err := cli.WriteImageInfo(&text, cli.FormatText, info, true); err != nil {
		t.Fatalf("WriteImageInfo text: %v", err)
	}
	got := text.String()
	if !strings.Contains(got, "DIFFID") || !strings.Contains(got, "CHAINID") || !strings.Contains(got, "ARCHIVE") {
		t.Errorf("table header missing:\n%s", got)
	}
	// The history command is JSON-only detail; the table carries identity and
	// size columns.
	if strings.Contains(got, "CREATED BY") || strings.Contains(got, "ADD rootfs.tar") || strings.Contains(got, "forged") {
		t.Errorf("table carries history text, want it omitted:\n%s", got)
	}
	if !strings.Contains(got, layer0Digest.Hex()[:12]) {
		t.Errorf("table missing abbreviated blob digest %s:\n%s", layer0Digest.Hex()[:12], got)
	}
	if strings.Contains(got, layer0Digest.Hex()) {
		t.Errorf("table carries a full digest, want 12-hex abbreviation:\n%s", got)
	}
	if strings.Contains(got, "diff_id\t") {
		t.Errorf("plain diff_id rows should be superseded by the table:\n%s", got)
	}
	if !strings.Contains(got, "3.4 MiB") || !strings.Contains(got, "512 B") {
		t.Errorf("human-readable sizes missing:\n%s", got)
	}
	if !strings.Contains(got, "rootfs") || !strings.Contains(got, "23.8 MiB") {
		t.Errorf("rootfs size row missing:\n%s", got)
	}
	// Publisher-authored text (the media type) is scrubbed: no C0 escape byte,
	// no single-byte C1 CSI reaches the terminal.
	if strings.ContainsAny(got, "\x1b\u009b") {
		t.Errorf("text output carries a raw control character:\n%q", got)
	}
	if !strings.Contains(got, "type+zstd") {
		t.Errorf("scrubbed media type tail missing:\n%s", got)
	}
	// The media type prints whole, not abbreviated.
	if !strings.Contains(got, "application/vnd.oci.image.layer.v1.tar+gzip") {
		t.Errorf("full media type missing from the table:\n%s", got)
	}

	var jsonBuf bytes.Buffer
	if err := cli.WriteImageInfo(&jsonBuf, cli.FormatJSON, info, true); err != nil {
		t.Fatalf("WriteImageInfo json: %v", err)
	}
	var out cli.ImageInfoOutput
	if err := json.Unmarshal(jsonBuf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if len(out.Layers) != 2 {
		t.Fatalf("json layers = %d, want 2", len(out.Layers))
	}
	l0, l1 := out.Layers[0], out.Layers[1]
	if l0.Index != 0 || l1.Index != 1 {
		t.Errorf("indexes = %d/%d, want 0/1", l0.Index, l1.Index)
	}
	if l0.Digest != layer0Digest.String() || l0.DiffID != info.Layers[0].DiffID.String() || l0.ChainID != info.Layers[0].ChainID.String() {
		t.Errorf("json layer 0 digests are not the full values: %#v", l0)
	}
	if l0.Size != 3565158 {
		t.Errorf("json layer 0 size = %d, want the numeric byte count", l0.Size)
	}
	// JSON keeps the original string; encoding/json escapes it safely.
	if l1.CreatedBy != info.Layers[1].CreatedBy {
		t.Errorf("json createdBy = %q, want the unmodified value", l1.CreatedBy)
	}
	if out.RootfsSize != 25000000 {
		t.Errorf("json rootfs_size = %d, want the numeric byte count", out.RootfsSize)
	}

	// The rootfs size row is part of the identity block, not layer detail: it
	// renders in the default text output too, without --layers.
	var plain bytes.Buffer
	if err := cli.WriteImageInfo(&plain, cli.FormatText, info, false); err != nil {
		t.Fatalf("WriteImageInfo text without layers: %v", err)
	}
	if !strings.Contains(plain.String(), "rootfs") || !strings.Contains(plain.String(), "23.8 MiB") {
		t.Errorf("default text output missing the rootfs size row:\n%s", plain.String())
	}
}

// TestRunInspectLayersFromDirE2E pulls a multi-layer image with history from
// an OCI layout and inspects it with --layers, entirely offline: the layer
// view must be served from the local store without any network access.
func TestRunInspectLayersFromDirE2E(t *testing.T) {
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

	lay, err := octest.BuildImage(t.TempDir(), octest.ImageSpec{
		Platform: ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH},
		Layers: []octest.LayerSpec{
			{Files: []octest.File{{Name: "etc/base.txt", Mode: 0o644, Body: "base\n"}}},
			{Files: []octest.File{{Name: "etc/app.txt", Mode: 0o644, Body: "app\n"}}, MediaType: ocispec.MediaTypeImageLayerZstd},
		},
		History: []ocispec.History{
			{CreatedBy: "ADD base /"},
			{CreatedBy: "ENV A=1", EmptyLayer: true},
			{CreatedBy: "ADD app /"},
		},
	})
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"--root", storeRoot, "pull", "--from-dir", lay.Dir, lay.Ref,
	}, &out, &errOut)
	if code != 0 {
		if strings.Contains(errOut.String(), "fs-verity") {
			t.Skipf("fs-verity unsupported on the test filesystem: %s", errOut.String())
		}
		t.Fatalf("pull exit = %d, stderr = %q", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{
		"--root", storeRoot, "inspect", "--layers", "--format", "json", lay.Ref,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("inspect --layers exit = %d, stderr = %q", code, errOut.String())
	}

	var got cli.ImageInfoOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v; out=%q", err, out.String())
	}
	if len(got.Layers) != 2 {
		t.Fatalf("layers = %d, want 2", len(got.Layers))
	}
	for i, l := range got.Layers {
		if l.DiffID != lay.DiffIDs[i].String() {
			t.Errorf("layer %d diffID = %s, want %s", i, l.DiffID, lay.DiffIDs[i])
		}
		if l.Size <= 0 {
			t.Errorf("layer %d size = %d, want > 0", i, l.Size)
		}
	}
	if got.Layers[0].ChainID != lay.DiffIDs[0].String() {
		t.Errorf("layer 0 chainID = %s, want its diffID %s", got.Layers[0].ChainID, lay.DiffIDs[0])
	}
	if got.Layers[0].CreatedBy != "ADD base /" || got.Layers[1].CreatedBy != "ADD app /" {
		t.Errorf("createdBy = %q/%q, want the non-empty history entries", got.Layers[0].CreatedBy, got.Layers[1].CreatedBy)
	}
	if got.Layers[1].MediaType != string(ocispec.MediaTypeImageLayerZstd) {
		t.Errorf("layer 1 mediaType = %q, want zstd", got.Layers[1].MediaType)
	}
	if want := int64(len("base\n") + len("app\n")); got.RootfsSize != want {
		t.Errorf("rootfs_size = %d, want %d (the merged tree's content bytes)", got.RootfsSize, want)
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{
		"--root", storeRoot, "inspect", "--layers", lay.Ref,
	}, &out, &errOut); code != 0 {
		t.Fatalf("inspect --layers (text) exit = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "DIFFID") || !strings.Contains(out.String(), lay.DiffIDs[0].Hex()[:12]) {
		t.Errorf("text table missing header or layer identity:\n%s", out.String())
	}
	if strings.Contains(out.String(), "ADD base /") {
		t.Errorf("text table carries history text, want it omitted:\n%s", out.String())
	}
}
