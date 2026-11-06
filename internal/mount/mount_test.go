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
	"time"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/ocicfs/internal/devtest"
	"github.com/foundriesio/ocicfs/internal/objectstore"
	"github.com/foundriesio/ocicfs/internal/rootfs"
	"github.com/foundriesio/ocicfs/internal/verity"
)

// mountBody is the fixture file's payload. It exceeds the inline threshold, so
// the EROFS stores it as a redirect to a store object and every read through
// the mount crosses the overlay's data-only lower layer.
const mountBody = "hello from mount - a payload well over the sixty-four byte inline threshold\n"

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

// TestOverlayOptionsPinTrustPosture pins the assembled overlay mount that
// carries the trust posture — the metadata lower, the data-only object lower,
// redirect and metacopy resolution, fs-verity required on every redirected
// object, and the source name mounts are identified by — so a refactor
// dropping any of them fails here.
func TestOverlayOptionsPinTrustPosture(t *testing.T) {
	got := overlayOptions("/run/ocicfs/mount-1", "/objects", false)
	want := "lowerdir=/run/ocicfs/mount-1::/objects,redirect_dir=on,metacopy=on,verity=require"
	if got != want {
		t.Fatalf("options = %q, want %q", got, want)
	}
	// A store without fs-verity requests no validation at all: verity=on
	// would fail every redirected object, whose metacopy names a digest.
	got = overlayOptions("/run/ocicfs/mount-1", "/objects", true)
	want = "lowerdir=/run/ocicfs/mount-1::/objects,redirect_dir=on,metacopy=on"
	if got != want {
		t.Fatalf("options without fs-verity = %q, want %q", got, want)
	}
	if overlaySource != "composefs" {
		t.Fatalf("overlay source = %q, want %q", overlaySource, "composefs")
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

// TestReadOnlyMountsComposefs drives the whole native assembly: content is
// served through a resolved redirect after the scratch metadata mountpoint
// was lazily detached, the mount is read-only, the image's in-use lock reads
// busy for the mount's whole lifetime even though the mounting process closed
// its own descriptors, and one unmount of the target releases the loop device
// and the lock.
func TestReadOnlyMountsComposefs(t *testing.T) {
	storeRoot, obj, image := buildMountFixture(t)
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
		Basedir:        obj.ViewDir(),
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

	// Content resolves through the redirect into the data-only lower even
	// though the scratch EROFS mountpoint is already detached: the overlay
	// holds its own references to the metadata tree.
	data, err := os.ReadFile(filepath.Join(target, "hello.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != mountBody {
		t.Fatalf("mounted file = %q", data)
	}
	if err := os.WriteFile(filepath.Join(target, "new"), []byte("nope"), 0o644); err == nil {
		t.Fatal("expected read-only mount to reject writes")
	}

	if len(loopsBacking(t, imagePath)) == 0 {
		t.Fatal("no loop device backs the mounted image")
	}
	// The in-use lock must outlive the mounting process's own descriptors —
	// ReadOnly already closed its image and loop handles — because the loop
	// device retains the locked file description. An exclusive non-blocking
	// probe, what gc runs before deleting, must report contention.
	probe, err := os.Open(imagePath) // #nosec G304 -- store object path under the test root
	if err != nil {
		t.Fatalf("Open probe: %v", err)
	}
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("close probe: %v", err)
		}
	})
	err = unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- a file descriptor always fits in int
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("probe flock = %v, want EWOULDBLOCK while mounted", err)
	}

	if err := Unmount(target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false

	// Loop teardown after the unmount is deferred kernel work, so both the
	// autoclear and the lock release are polled rather than asserted once.
	waitFor(t, "loop autoclear after unmount", func() bool {
		return len(loopsBacking(t, imagePath)) == 0
	})
	waitFor(t, "image lock release after unmount", func() bool {
		return unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil // #nosec G115 -- a file descriptor always fits in int
	})

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("target not empty after unmount: %v", entries)
	}
}

// TestReadOnlyRefusesFsverityMismatch: the expected digest is enforced on the
// locked descriptor before any device or mount exists, so a mismatch must
// fail with no loop device attached. The engine pre-measures on its own path;
// this pins that ReadOnly does not depend on that.
func TestReadOnlyRefusesFsverityMismatch(t *testing.T) {
	storeRoot, obj, image := buildMountFixture(t)
	imagePath, err := obj.ObjectPath(image.Descriptor.Digest)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	target := filepath.Join(storeRoot, "mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	err = ReadOnly(context.Background(), Spec{
		ImagePath:      imagePath,
		Basedir:        obj.ViewDir(),
		Target:         target,
		FsverityDigest: digest.SHA256.FromString("wrong").String(),
	})
	if err == nil || !strings.Contains(err.Error(), "fs-verity digest mismatch") {
		t.Fatalf("ReadOnly = %v, want the fd-measure mismatch", err)
	}
	if devs := loopsBacking(t, imagePath); len(devs) != 0 {
		t.Fatalf("loop devices %v back the refused image, want none", devs)
	}
}

// TestReadOnlyRefusesVerityLessObject proves object-level fs-verity
// enforcement is active on the data plane: a backing object without
// fs-verity enabled is refused, even with byte-identical content. The
// store's objects are fs-verity-immutable, so the fixture swaps the object's
// view name to a plain copy of the same bytes. The overlay's require-vs-on
// posture itself is pinned by TestOverlayOptionsPinTrustPosture.
func TestReadOnlyRefusesVerityLessObject(t *testing.T) {
	storeRoot, obj, image := buildMountFixture(t)
	imagePath, err := obj.ObjectPath(image.Descriptor.Digest)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}

	fsv, err := obj.VerityDigest(context.Background(), digest.SHA256.FromString(mountBody))
	if err != nil {
		t.Fatalf("VerityDigest: %v", err)
	}
	hex := strings.TrimPrefix(fsv, "sha256:")
	entry := filepath.Join(obj.ViewDir(), hex[:2], hex[2:])
	if err := os.Remove(entry); err != nil {
		t.Fatalf("remove view entry: %v", err)
	}
	if err := os.WriteFile(entry, []byte(mountBody), 0o600); err != nil {
		t.Fatalf("write verity-less object: %v", err)
	}

	target := filepath.Join(storeRoot, "mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	err = ReadOnly(context.Background(), Spec{
		ImagePath:      imagePath,
		Basedir:        obj.ViewDir(),
		Target:         target,
		FsverityDigest: image.FsverityDigest,
	})
	if skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	}
	if err == nil {
		t.Cleanup(func() {
			if err := Unmount(target); err != nil && !errors.Is(err, unix.EINVAL) {
				t.Errorf("cleanup unmount: %v", err)
			}
		})
		if _, err := os.ReadFile(filepath.Join(target, "hello.txt")); err == nil { // #nosec G304 -- test mount path
			t.Fatal("read of a verity-less backing object succeeded")
		}
	}
}

// buildMountFixture assembles a one-file image on a verity-capable store and
// returns the store root, the object store, and the built image. The file
// payload is over the inline threshold, so mounts of the image exercise the
// data plane, not just the metadata.
func buildMountFixture(t *testing.T) (string, *objectstore.Store, rootfs.Image) {
	t.Helper()
	if verity.Mode == verity.ModeOff {
		t.Skip("a build made in off mode gives a new store no fs-verity")
	}
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

	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	if err := obj.CheckVerity(); err != nil {
		t.Fatalf("CheckVerity: %v", err)
	}
	layer := tarLayer(t, "hello.txt", 0o644, mountBody)
	openLayer := func(int) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layer)), nil
	}
	image, err := rootfs.Assemble(context.Background(), obj, openLayer, []digest.Digest{digest.SHA256.FromBytes(layer)})
	if devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return storeRoot, obj, image
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

// loopsBacking returns the sysfs entries of the loop devices whose backing
// file is path.
func loopsBacking(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	if err != nil {
		t.Fatalf("glob loop backing files: %v", err)
	}
	var devs []string
	for _, m := range matches {
		// A device detaching mid-scan loses its backing_file node; skip it.
		data, err := os.ReadFile(m) // #nosec G304 -- sysfs loop attribute path
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == path {
			devs = append(devs, m)
		}
	}
	return devs
}

// waitFor polls cond until it holds, failing the test after a short deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMetadataOnlyRejectsInvalidSpec: the argument checks fire before any
// filesystem work, so they need no verity environment.
func TestMetadataOnlyRejectsInvalidSpec(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{"empty image", Spec{Target: "/mnt", FsverityDigest: digest.SHA256.FromString("verity").String()}, "empty image path"},
		{"empty target", Spec{ImagePath: "/image", FsverityDigest: digest.SHA256.FromString("verity").String()}, "empty target"},
		{"empty digest", Spec{ImagePath: "/image", Target: "/mnt"}, "empty fs-verity digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MetadataOnly(context.Background(), tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("MetadataOnly = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestMetadataOnlyRefusesFsverityMismatch: the digest is enforced on the very
// descriptor the loop device would attach, so a mismatch must fail before any
// attach -- this is the enforcement point on the metadata-only path, and a
// caller-side pre-measure must not be what keeps it honest.
func TestMetadataOnlyRefusesFsverityMismatch(t *testing.T) {
	dir, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	path := filepath.Join(dir, "image.erofs")
	if err := os.WriteFile(path, []byte("metadata-only mismatch fixture"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0) // #nosec G304 -- test fixture path under t-managed dir
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	enableErr := verity.Enable(f)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if devtest.IsVerityUnsupported(enableErr) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", enableErr)
	}
	if enableErr != nil {
		t.Fatalf("Enable: %v", enableErr)
	}

	err = MetadataOnly(context.Background(), Spec{
		ImagePath:      path,
		Target:         t.TempDir(),
		FsverityDigest: digest.SHA256.FromString("wrong").String(),
	})
	if err == nil || !strings.Contains(err.Error(), "fs-verity digest mismatch") {
		t.Fatalf("MetadataOnly = %v, want the fd-measure mismatch", err)
	}
}

// TestLockImageMarksInUse pins the in-use protocol's mount side: while the
// image lock is held, an exclusive non-blocking probe on the same inode —
// what gc runs before deleting — must report contention, and must succeed
// again once the lock is gone.
func TestLockImageMarksInUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.erofs")
	if err := os.WriteFile(path, []byte("lock fixture"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	img, err := lockImage(path)
	if err != nil {
		t.Fatalf("lockImage: %v", err)
	}

	probe, err := os.Open(path) // #nosec G304 -- test fixture path under t-managed dir
	if err != nil {
		t.Fatalf("Open probe: %v", err)
	}
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("close probe: %v", err)
		}
	})
	err = unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- a file descriptor always fits in int
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("probe flock = %v, want EWOULDBLOCK while the image lock is held", err)
	}

	if err := img.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor always fits in int
		t.Fatalf("probe flock after release = %v, want success", err)
	}
}

// TestLockLinkedRefusesRemovedImage covers the unlink race with gc: an image
// whose name vanished between the open and the lock must be refused with the
// pull remedy rather than mounted from its nameless inode.
func TestLockLinkedRefusesRemovedImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.erofs")
	if err := os.WriteFile(path, []byte("removed fixture"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	img, err := os.Open(path) // #nosec G304 -- test fixture path under t-managed dir
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := img.Close(); err != nil {
			t.Errorf("close image: %v", err)
		}
	})
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	err = lockLinked(img)
	if err == nil || !strings.Contains(err.Error(), "removed by gc") {
		t.Fatalf("lockLinked = %v, want the removed-by-gc refusal", err)
	}
}
