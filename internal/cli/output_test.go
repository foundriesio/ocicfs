// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/foundriesio/ocicfs/pkg/ocicfs"
)

// TestWriteImagesHeldOnlyRef: an image no reference names renders "-" in the
// reference column of the text listing and an empty ref in JSON.
func TestWriteImagesHeldOnlyRef(t *testing.T) {
	imgs := []ocicfs.Image{{Digest: "sha256:abc", Size: 7}}

	var text bytes.Buffer
	if err := WriteImages(&text, FormatText, imgs); err != nil {
		t.Fatalf("WriteImages text: %v", err)
	}
	if got, want := text.String(), "-\tsha256:abc\t7\n"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}

	var js bytes.Buffer
	if err := WriteImages(&js, FormatJSON, imgs); err != nil {
		t.Fatalf("WriteImages json: %v", err)
	}
	var decoded []ImageOutput
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("json = %q: %v", js.String(), err)
	}
	if len(decoded) != 1 || decoded[0] != (ImageOutput{Ref: "", Digest: "sha256:abc", Size: 7}) {
		t.Fatalf("json = %#v", decoded)
	}
}

// TestWriteFsckReportText: a failed object with no detail ends its line at
// the name.
func TestWriteFsckReportText(t *testing.T) {
	report := &ocicfs.FsckReport{Images: []ocicfs.FsckImage{{
		Digest: "sha256:abc", Platform: "linux/arm64/v8",
		Objects: []ocicfs.FsckObject{{Fsverity: "sha256:def", Problem: ocicfs.FsckMissing}},
	}}}
	var text bytes.Buffer
	if err := WriteFsckReport(&text, FormatText, report); err != nil {
		t.Fatalf("WriteFsckReport: %v", err)
	}
	if !strings.Contains(text.String(), " fsverity sha256:def\n") {
		t.Fatalf("text = %q", text.String())
	}
}

// TestWriteFsckReportPullAgain: an image whose binding a repair removed is
// marked for a pull, and the last line says so.
func TestWriteFsckReportPullAgain(t *testing.T) {
	removed := true
	report := &ocicfs.FsckReport{Images: []ocicfs.FsckImage{{
		Digest: "sha256:abc", Platform: "linux/arm64/v8", BindingRemoved: &removed,
		Objects: []ocicfs.FsckObject{{Fsverity: "sha256:def", Problem: ocicfs.FsckMissing}},
	}}}
	var text bytes.Buffer
	if err := WriteFsckReport(&text, FormatText, report); err != nil {
		t.Fatalf("WriteFsckReport: %v", err)
	}
	if got := text.String(); !strings.Contains(got, "sha256:abc linux/arm64/v8 PULL AGAIN") || !strings.Contains(got, "pull each image marked PULL AGAIN") {
		t.Fatalf("text = %q", got)
	}
}
