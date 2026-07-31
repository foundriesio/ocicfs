// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package composefs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// objectsFixture builds a V1 image through WriteImage covering every entry
// kind the enumeration must handle: two by-digest files sharing one object, a
// distinct by-digest file, a hardlink pair, an inline file, a symlink, and
// nested directories. It returns the image path and the expected object
// names.
func objectsFixture(t *testing.T) (string, []string) {
	t.Helper()
	lookPathOrSkip(t, "mkcomposefs") // WriteImage invokes it internally

	shared := digest.SHA256.FromString("shared object content")
	sharedVerity := digest.SHA256.FromString("shared verity")
	distinct := digest.SHA256.FromString("distinct object content")
	distinctVerity := digest.SHA256.FromString("distinct verity")
	entries := []Entry{
		{Path: "/usr", Mode: os.ModeDir | 0o755},
		{Path: "/usr/bin", Mode: os.ModeDir | 0o755},
		{Path: "/usr/bin/app", Mode: 0o755, Size: 100, Digest: shared, Verity: sharedVerity},
		{Path: "/usr/bin/copy", Mode: 0o644, Size: 100, Digest: shared, Verity: sharedVerity},
		{Path: "/usr/bin/other", Mode: 0o644, Size: 42, Digest: distinct, Verity: distinctVerity},
		{Path: "/usr/bin/alias", Hardlink: "/usr/bin/app"},
		{Path: "/etc", Mode: os.ModeDir | 0o755},
		{Path: "/etc/small", Mode: 0o644, Content: []byte("abc")},
		{Path: "/etc/link", Mode: os.ModeSymlink | 0o777, Linkname: "../usr/bin/app"},
	}
	out := filepath.Join(t.TempDir(), "img.erofs")
	if err := WriteImage(context.Background(), out, entries); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}

	want := []string{
		verityName(t, sharedVerity),
		verityName(t, distinctVerity),
	}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	return out, want
}

func verityName(t *testing.T, d digest.Digest) string {
	t.Helper()
	enc := d.Encoded()
	return enc[:2] + "/" + enc[2:]
}

func TestReferencedObjectsEnumeratesRedirects(t *testing.T) {
	img, want := objectsFixture(t)

	got, err := ReferencedObjects(context.Background(), img)
	if err != nil {
		t.Fatalf("ReferencedObjects: %v", err)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ReferencedObjects = %v, want %v", got, want)
	}
}

// TestReferencedObjectsMatchesComposefsInfo is the conformance oracle: gc's
// deletion decisions ride on the go-erofs read, so its enumeration of a built
// image must equal what the pinned C tool reports (modulo the leading slash
// composefs-info strips and the enumeration canonicalizes away).
func TestReferencedObjectsMatchesComposefsInfo(t *testing.T) {
	cfsInfo := lookPathOrSkip(t, "composefs-info")
	img, _ := objectsFixture(t)

	got, err := ReferencedObjects(context.Background(), img)
	if err != nil {
		t.Fatalf("ReferencedObjects: %v", err)
	}

	out, err := exec.Command(cfsInfo, "objects", img).Output() // #nosec G204 -- fixed binary, test fixture path
	if err != nil {
		t.Fatalf("composefs-info objects: %v", err)
	}
	var want []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/"))
		if line != "" {
			want = append(want, line)
		}
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ReferencedObjects = %v, composefs-info objects = %v", got, want)
	}
}
