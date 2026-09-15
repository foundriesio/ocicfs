// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./pkg/knarr")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./pkg/knarr: %v\n%s", err, out)
	}

	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "github.com/foundriesio/knarr/internal/ociremote":
			t.Errorf("pkg/knarr depends on internal/ociremote")
		case strings.HasPrefix(dep, "github.com/google/go-containerregistry/"):
			t.Errorf("pkg/knarr depends on registry client package %s", dep)
		}
	}
}
