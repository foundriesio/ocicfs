// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRunHoldArgValidation(t *testing.T) {
	for _, args := range [][]string{{"hold"}, {"hold", "a", "b"}, {"release"}, {"release", "a", "b"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) exit = %d, want 2 (usage)", args, code)
		}
	}
}

func TestRunHoldNeverPulledIsNotFound(t *testing.T) {
	root := t.TempDir()
	ref := digest.SHA256.FromString("absent").String()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--root", root, "hold", ref}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "knarr pull") {
		t.Errorf("stderr = %q, want it to suggest knarr pull", errOut.String())
	}
}

func TestRunHoldReleaseRemoveCycle(t *testing.T) {
	root := t.TempDir()
	imageRoot := digest.SHA256.FromString("root")
	ref := "registry.example.com/app@" + imageRoot.String()
	platform := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	writeCLIImage(t, root, ref, imageRoot, 72, platform)

	// hold --format json emits the machine-readable object on stdout.
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--root", root, "hold", "--holder", "cfs", "--format", "json", ref}, &out, &errOut); code != 0 {
		t.Fatalf("hold exit = %d, stderr = %q", code, errOut.String())
	}
	var held struct {
		Ref    string `json:"ref"`
		Holder string `json:"holder"`
		Held   bool   `json:"held"`
	}
	if err := json.Unmarshal(out.Bytes(), &held); err != nil {
		t.Fatalf("hold stdout is not JSON: %v; out=%q", err, out.String())
	}
	if held.Ref != ref || held.Holder != "cfs" || !held.Held {
		t.Fatalf("hold JSON = %#v", held)
	}

	// A plain remove declines while held, naming the holder.
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"--root", root, "remove", ref}, &out, &errOut); code != 1 {
		t.Fatalf("remove-while-held exit = %d, want 1; stderr = %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "held by cfs") {
		t.Errorf("stderr = %q, want it to name the holder", errOut.String())
	}

	// Releasing then removing succeeds.
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"--root", root, "release", "--holder", "cfs", ref}, &out, &errOut); code != 0 {
		t.Fatalf("release exit = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "released") {
		t.Errorf("release stdout = %q, want a released confirmation", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"--root", root, "remove", ref}, &out, &errOut); code != 0 {
		t.Fatalf("remove-after-release exit = %d, stderr = %q", code, errOut.String())
	}
}

func TestRunRemoveForceClearsHold(t *testing.T) {
	root := t.TempDir()
	imageRoot := digest.SHA256.FromString("root")
	ref := "registry.example.com/app@" + imageRoot.String()
	platform := ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	writeCLIImage(t, root, ref, imageRoot, 72, platform)

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--root", root, "hold", ref}, &out, &errOut); code != 0 {
		t.Fatalf("hold exit = %d, stderr = %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"--root", root, "remove", "--force", ref}, &out, &errOut); code != 0 {
		t.Fatalf("forced remove exit = %d, stderr = %q", code, errOut.String())
	}
}
