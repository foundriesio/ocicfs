// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/foundriesio/knarr/pkg/knarr"
)

// TestWriteImagesHeldOnlyRef: an image no reference names renders "-" in the
// reference column of the text listing and an empty ref in JSON.
func TestWriteImagesHeldOnlyRef(t *testing.T) {
	imgs := []knarr.Image{{Digest: "sha256:abc", Size: 7}}

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
