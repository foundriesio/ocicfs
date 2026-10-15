// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package verity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foundriesio/ocicfs/internal/devtest"
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

// TestDigestMatchesKernelAndTools cross-checks the userspace Digest against
// three independent oracles: the kernel fs-verity measure, composefs-info
// measure-file, and fsverity digest.
func TestDigestMatchesKernelAndTools(t *testing.T) {
	for _, tool := range []string{"composefs-info", "fsverity"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	root, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable filesystem unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "small", data: []byte("fs-verity")},
		{name: "multi-block", data: make([]byte, 3*BlockSize+17)},
		{name: "multi-level", data: make([]byte, 129*BlockSize)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := Digest(context.Background(), bytes.NewReader(tt.data))
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}

			p := filepath.Join(root, tt.name)
			if err := os.WriteFile(p, tt.data, 0o400); err != nil {
				t.Fatal(err)
			}
			// composefs-info 1.0.8 computes a zero-length file's digest as if
			// it contained one zero block, diverging from the kernel
			// algorithm; the kernel measure below is the authority for empty
			// files.
			if len(tt.data) > 0 {
				out, err := exec.Command("composefs-info", "measure-file", p).CombinedOutput() // #nosec G204 -- fixed binary measuring a test-owned file
				if err != nil {
					t.Fatalf("composefs-info measure-file: %v: %s", err, out)
				}
				composefsDigest := "sha256:" + strings.TrimSpace(string(out))
				if composefsDigest != want {
					t.Fatalf("composefs-info = %q, Digest = %q", composefsDigest, want)
				}
			}

			// fsverity digest computes the digest from content in userspace,
			// matching the kernel algorithm on every size including empty
			// files. Fixed sha256/4096 flags keep it deterministic regardless
			// of host page size; its output is "sha256:<hex> <path>".
			out, err := exec.Command("fsverity", "digest", "--hash-alg=sha256", "--block-size=4096", p).CombinedOutput() // #nosec G204 -- fixed binary measuring a test-owned file
			if err != nil {
				t.Fatalf("fsverity digest: %v: %s", err, out)
			}
			fields := strings.Fields(string(out))
			if len(fields) == 0 {
				t.Fatalf("fsverity digest: unexpected output %q", out)
			}
			fsverityDigest := fields[0]
			if fsverityDigest != want {
				t.Fatalf("fsverity = %q, Digest = %q", fsverityDigest, want)
			}

			f, err := os.Open(p) //nolint:gosec // p is a test-owned file under the verity root
			if err != nil {
				t.Fatal(err)
			}
			if err := Enable(f); err != nil {
				if closeErr := f.Close(); closeErr != nil {
					t.Errorf("close test file: %v", closeErr)
				}
				if devtest.IsVerityUnsupported(err) {
					t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
				}
				t.Fatalf("Enable: %v", err)
			}
			measured, measureErr := Measure(f)
			closeErr := f.Close()
			if measureErr != nil {
				t.Fatalf("Measure: %v", measureErr)
			}
			if closeErr != nil {
				t.Fatalf("close test file: %v", closeErr)
			}
			if measured != want {
				t.Fatalf("Measure = %q, Digest = %q", measured, want)
			}
		})
	}
}
