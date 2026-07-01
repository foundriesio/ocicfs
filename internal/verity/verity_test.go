// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package verity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundriesio/knarr/internal/devtest"
)

// TestEnableMeasure enables fs-verity on a file and measures it. It runs on the
// dev container's verity-capable filesystem and skips when that is unavailable
// (for example running outside the privileged dev container).
func TestEnableMeasure(t *testing.T) {
	root, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable filesystem unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	p := filepath.Join(root, "obj")
	if err := os.WriteFile(p, []byte("hello fs-verity"), 0o400); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p) //nolint:gosec // p is a file just created under the verity test root
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if err := Enable(f); err != nil {
		if devtest.IsVerityUnsupported(err) {
			t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
		}
		t.Fatalf("Enable: %v", err)
	}

	got, err := Measure(f)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+2*sha256Size {
		t.Fatalf("Measure = %q, want sha256:<64 hex>", got)
	}
}
