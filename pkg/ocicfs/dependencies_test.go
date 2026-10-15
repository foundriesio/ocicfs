// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package ocicfs

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./pkg/ocicfs")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./pkg/ocicfs: %v\n%s", err, out)
	}

	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "github.com/foundriesio/ocicfs/internal/ociremote":
			t.Errorf("pkg/ocicfs depends on internal/ociremote")
		case strings.HasPrefix(dep, "github.com/google/go-containerregistry/"):
			t.Errorf("pkg/ocicfs depends on registry client package %s", dep)
		}
	}
}
