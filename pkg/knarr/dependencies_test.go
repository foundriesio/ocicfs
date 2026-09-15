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

	const allowedRegistryDependency = "github.com/google/go-containerregistry/pkg/name"
	allowed := 0
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "github.com/foundriesio/knarr/internal/ociremote":
			t.Errorf("pkg/knarr depends on internal/ociremote")
		case dep == allowedRegistryDependency:
			allowed++
		case strings.HasPrefix(dep, "github.com/google/go-containerregistry/"):
			t.Errorf("pkg/knarr has unexpected registry dependency %s", dep)
		}
	}
	if allowed != 1 {
		t.Errorf("pkg/knarr dependency count for %s = %d, want 1", allowedRegistryDependency, allowed)
	}
}
