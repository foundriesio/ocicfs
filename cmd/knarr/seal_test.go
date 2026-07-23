// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/foundriesio/knarr/internal/cli"
	"github.com/foundriesio/knarr/internal/ociseal"
)

func TestWriteSealResult(t *testing.T) {
	const repo = "registry.example.com/team/app"
	result := ociseal.Result{
		Original: digest.SHA256.FromString("original"),
		Sealed:   digest.SHA256.FromString("sealed"),
		Platforms: []ociseal.PlatformResult{{
			Platform:       ocispec.Platform{OS: "linux", Architecture: "amd64"},
			Original:       digest.SHA256.FromString("child"),
			Sealed:         digest.SHA256.FromString("sealed child"),
			MergedFsverity: digest.SHA256.FromString("merged").String(),
		}},
		Repository: repo,
	}
	originalRef := repo + "@" + result.Original.String()
	sealedRef := repo + "@" + result.Sealed.String()

	var text bytes.Buffer
	if err := cli.WriteSealResult(&text, cli.FormatText, result); err != nil {
		t.Fatalf("WriteSealResult text: %v", err)
	}
	want := "original  " + originalRef + "\n" +
		"sealed    " + sealedRef + "\n" +
		"fsverity  " + result.Platforms[0].MergedFsverity + "\n"
	if text.String() != want {
		t.Fatalf("text output = %q, want %q", text.String(), want)
	}

	var jsonOutput bytes.Buffer
	if err := cli.WriteSealResult(&jsonOutput, cli.FormatJSON, result); err != nil {
		t.Fatalf("WriteSealResult JSON: %v", err)
	}
	var decoded cli.SealResultOutput
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if decoded.Original != result.Original.String() ||
		decoded.Sealed != result.Sealed.String() ||
		len(decoded.Platforms) != 1 ||
		decoded.Platforms[0].Fsverity != result.Platforms[0].MergedFsverity {
		t.Fatalf("JSON output = %#v", decoded)
	}
	// The reference fields are what a chained call consumes, so they must name
	// the digest beside them in the repository seal pushed to. The platform row
	// reports both sides because for an index the top-level digests are the
	// index's, not this platform's.
	platform := result.Platforms[0]
	if decoded.OriginalRef != originalRef || decoded.SealedRef != sealedRef ||
		decoded.Platforms[0].OriginalManifest != platform.Original.String() ||
		decoded.Platforms[0].OriginalManifestRef != repo+"@"+platform.Original.String() ||
		decoded.Platforms[0].SealedManifest != platform.Sealed.String() ||
		decoded.Platforms[0].SealedManifestRef != repo+"@"+platform.Sealed.String() {
		t.Fatalf("JSON references = %#v", decoded)
	}
}

// TestWriteSealResultWithoutRepository covers a result that carries no
// repository: the reference fields fall back to the bare digest rather than
// rendering a reference with an empty repository.
func TestWriteSealResultWithoutRepository(t *testing.T) {
	result := ociseal.Result{
		Original: digest.SHA256.FromString("original"),
		Sealed:   digest.SHA256.FromString("sealed"),
	}
	var out bytes.Buffer
	if err := cli.WriteSealResult(&out, cli.FormatJSON, result); err != nil {
		t.Fatalf("WriteSealResult JSON: %v", err)
	}
	var decoded cli.SealResultOutput
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if decoded.OriginalRef != result.Original.String() || decoded.SealedRef != result.Sealed.String() {
		t.Fatalf("JSON references = %#v", decoded)
	}
}
