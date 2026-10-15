// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package zstdchunked

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./internal/zstdchunked")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./internal/zstdchunked: %v\n%s", err, out)
	}

	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "github.com/foundriesio/ocicfs/internal/oci":
			t.Errorf("internal/zstdchunked depends on internal/oci")
		case strings.HasPrefix(dep, "github.com/google/go-containerregistry/"):
			t.Errorf("internal/zstdchunked depends on registry client package %s", dep)
		}
	}
}
