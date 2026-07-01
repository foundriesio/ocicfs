// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package mount

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/rootfs"
)

func TestReadOnlyRejectsInvalidSpec(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{"empty image", Spec{Basedir: "/objects", Target: "/mnt", FsverityDigest: digest.SHA256.FromString("verity").String()}, "empty image path"},
		{"bad digest", Spec{ImagePath: "/image", Basedir: "/objects", Target: "/mnt", FsverityDigest: "sha512:abc"}, "fs-verity digest"},
		{"bad basedir", Spec{ImagePath: "/image", Basedir: "/objects:extra", Target: "/mnt", FsverityDigest: digest.SHA256.FromString("verity").String()}, "basedir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ReadOnly(context.Background(), tt.spec)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestComposefsOptionsPinTrustPosture pins the option string that carries the
// trust posture — expected image digest, fs-verity required on redirected
// objects, read-only — so a refactor dropping any of them fails here.
func TestComposefsOptionsPinTrustPosture(t *testing.T) {
	fsv := digest.SHA256.FromString("verity")
	opts, err := composefsOptions(Spec{
		ImagePath:      "/image",
		Basedir:        "/objects",
		Target:         "/mnt",
		FsverityDigest: fsv.String(),
	})
	if err != nil {
		t.Fatalf("composefsOptions: %v", err)
	}
	want := "basedir=/objects,digest=" + fsv.Hex() + ",verity,ro"
	if opts != want {
		t.Fatalf("options = %q, want %q", opts, want)
	}
}

// TestUnmountNotMounted: a target with no mount must surface an error matching
// ErrNotMounted so idempotent teardown paths can detect it with errors.Is.
func TestUnmountNotMounted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("umount2 needs privileges")
	}
	err := Unmount(t.TempDir())
	if !errors.Is(err, ErrNotMounted) {
		t.Fatalf("Unmount = %v, want an ErrNotMounted match", err)
	}
}

func TestReadOnlyMountsComposefs(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	if _, err := exec.LookPath("mount.composefs"); err != nil {
		t.Skip("mount.composefs not found in PATH")
	}

	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	if err := obj.CheckVerity(); err != nil {
		t.Fatalf("CheckVerity: %v", err)
	}
	layer := tarLayer(t, "hello.txt", 0o644, "hello from mount\n")
	image, err := rootfs.Assemble(context.Background(), obj, []io.Reader{bytes.NewReader(layer)}, []digest.Digest{digest.SHA256.FromBytes(layer)})
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imagePath, err := obj.ObjectPath(image.Descriptor.Digest)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}

	target := filepath.Join(storeRoot, "mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	mounted := false
	t.Cleanup(func() {
		if !mounted {
			return
		}
		if err := Unmount(target); err != nil && !errors.Is(err, unix.EINVAL) {
			t.Errorf("cleanup unmount: %v", err)
		}
	})

	err = ReadOnly(context.Background(), Spec{
		ImagePath:      imagePath,
		Basedir:        obj.ObjectsDir(),
		Target:         target,
		FsverityDigest: image.FsverityDigest,
	})
	if skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("ReadOnly: %v", err)
	}
	mounted = true

	data, err := os.ReadFile(filepath.Join(target, "hello.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello from mount\n" {
		t.Fatalf("mounted file = %q", data)
	}
	if err := os.WriteFile(filepath.Join(target, "new"), []byte("nope"), 0o644); err == nil {
		t.Fatal("expected read-only mount to reject writes")
	}
	if err := Unmount(target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("target not empty after unmount: %v", entries)
	}
}

func tarLayer(t *testing.T, name string, mode int64, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatalf("tar root: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func skipMountEnvironment(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Operation not permitted") ||
		strings.Contains(msg, "Permission denied")
}
