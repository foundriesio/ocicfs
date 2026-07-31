// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/imagestore"
	"github.com/foundriesio/knarr/internal/objectstore"
	"github.com/foundriesio/knarr/internal/ociseal"
	"github.com/foundriesio/knarr/internal/octest"
)

func TestMountFromPulledImageE2E(t *testing.T) {
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

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
		octest.File{Name: "etc/config.link", Link: "etc/config.txt"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
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
		if err := eng.Unmount(context.Background(), target); err != nil {
			t.Errorf("cleanup unmount: %v", err)
		}
	})

	err = eng.Mount(context.Background(), lay.Ref, target)
	if skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	mounted = true

	data, err := os.ReadFile(filepath.Join(target, "etc", "config.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data, []byte("hello from knarr\n")) {
		t.Fatalf("mounted file = %q", data)
	}

	// The layer's hardlink must surface as a real shared inode, not an
	// independent copy of the file.
	var st1, st2 unix.Stat_t
	if err := unix.Stat(filepath.Join(target, "etc", "config.txt"), &st1); err != nil {
		t.Fatalf("stat config.txt: %v", err)
	}
	if err := unix.Stat(filepath.Join(target, "etc", "config.link"), &st2); err != nil {
		t.Fatalf("stat config.link: %v", err)
	}
	if st1.Ino != st2.Ino || st1.Nlink != 2 {
		t.Fatalf("hardlink inodes = %d/%d nlink = %d, want one shared inode with nlink 2", st1.Ino, st2.Ino, st1.Nlink)
	}

	if err := eng.Unmount(context.Background(), target); err != nil {
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

func TestMountRefusesBindingFsverityMismatch(t *testing.T) {
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

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	tamperBindingDigest(t, octest.BindingPath(storeRoot, lay.Root, lay.Platform), digest.SHA256.FromString("wrong").String())

	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if err == nil {
		t.Fatal("expected mount to fail")
	}
	if !strings.Contains(err.Error(), "fs-verity digest mismatch") {
		t.Fatalf("error = %v, want fs-verity mismatch", err)
	}
}

func tamperBindingDigest(t *testing.T, path, replacement string) {
	t.Helper()
	rewriteBinding(t, path, func(rootfs map[string]any) {
		fsverityDoc, ok := rootfs["fsverity"].(map[string]any)
		if !ok {
			t.Fatalf("binding fsverity has unexpected shape")
		}
		fsverityDoc["digest"] = replacement
	})
}

// rewriteBinding rewrites the rootfs section of the binding file at path
// through mutate.
func rewriteBinding(t *testing.T, path string, mutate func(rootfs map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test binding path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	rootfsDoc, ok := doc["rootfs"].(map[string]any)
	if !ok {
		t.Fatalf("binding rootfs has unexpected shape: %s", data)
	}
	mutate(rootfsDoc)
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func skipMountEnvironment(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Operation not permitted") ||
		strings.Contains(msg, "Permission denied")
}

// TestMountUnpulledReturnsErrNotFound: mounting a ref that was never pulled
// must fail with an error matching the exported ErrNotFound, so an embedder
// can branch on it without string matching.
func TestMountUnpulledReturnsErrNotFound(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := "app@" + digest.SHA256.FromString("never pulled").String()
	err = eng.Mount(context.Background(), ref, t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Mount = %v, want an ErrNotFound match", err)
	}
}

// TestMountOldTupleBindingReportsNoImage: a binding carrying the superseded
// object-store tuple must surface the same binding-miss guidance as a missing
// binding, not a raw validation error, so an old cached image reads as a
// cache miss to be rebuilt.
func TestMountOldTupleBindingReportsNoImage(t *testing.T) {
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

	// A real pull, so the metadata chain resolves and the mount reaches the
	// binding lookup; only the binding's tuple is downgraded.
	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	downgradeBindingTuple(t, octest.BindingPath(storeRoot, lay.Root, lay.Platform))

	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Mount = %v, want an ErrNotFound match", err)
	}
	if !strings.Contains(err.Error(), "no composefs image for this platform") {
		t.Fatalf("Mount = %v, want the binding-miss wording", err)
	}
}

// TestPullRebuildsOldTupleBindingE2E: a re-pull over a binding written under the
// superseded object-store tuple treats it as a cache miss and rebuilds a fresh
// binding under the current tuple.
func TestPullRebuildsOldTupleBindingE2E(t *testing.T) {
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

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	downgradeBindingTuple(t, octest.BindingPath(storeRoot, lay.Root, lay.Platform))
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); err != nil {
		t.Fatalf("re-pull over old-tuple binding: %v", err)
	}

	img, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	b, err := img.Get(lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("Get after rebuild: %v", err)
	}
	if b.ObjectStorePath != "objects/fsverity-sha256-12" {
		t.Fatalf("object store path = %q after rebuild, want the current tuple", b.ObjectStorePath)
	}
}

// downgradeBindingTuple rewrites the binding at path to carry the superseded
// sha256 object-store tuple, simulating a cache entry from before the fs-verity
// view scheme.
func downgradeBindingTuple(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test rewrites a path it wrote
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	data = []byte(strings.NewReplacer(
		`"algorithm":"fsverity-sha256-12"`, `"algorithm":"sha256"`,
		`"path":"objects/fsverity-sha256-12"`, `"path":"objects/sha256"`,
	).Replace(string(data)))
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G703 -- test rewrites a path it wrote
		t.Fatalf("write binding: %v", err)
	}
}

// TestMountMissingObjectReturnsErrNotFound: a binding whose composefs object
// was removed from the store must fail with an ErrNotFound match, not a bare
// stat error.
func TestMountMissingObjectReturnsErrNotFound(t *testing.T) {
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

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	img, err := imagestore.New(storeRoot)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	b, err := img.Get(lay.Root, lay.Platform)
	if err != nil {
		t.Fatalf("Get binding: %v", err)
	}
	obj, err := objectstore.New(storeRoot)
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	path, err := obj.ObjectPath(b.Descriptor.Digest)
	if err != nil {
		t.Fatalf("ObjectPath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove composefs object: %v", err)
	}

	err = eng.Mount(context.Background(), lay.Ref, t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Mount = %v, want an ErrNotFound match", err)
	}
	if !strings.Contains(err.Error(), "image object missing") {
		t.Fatalf("error = %v, want the missing-object cause preserved", err)
	}
}

// TestUnmountNotMountedReturnsErrNotMounted: unmounting a directory with no
// mount must fail with an ErrNotMounted match so teardown paths can treat it
// as already done.
func TestUnmountNotMountedReturnsErrNotMounted(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("umount2 needs privileges")
	}
	eng, err := New(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = eng.Unmount(context.Background(), t.TempDir())
	if !errors.Is(err, ErrNotMounted) {
		t.Fatalf("Unmount = %v, want an ErrNotMounted match", err)
	}
}

// TestMountMetadataOnlyConsumerOverlayE2E exercises the metadata-only mount
// the way a snapshotter consumes it: mount the verified EROFS metadata alone,
// then assemble a single writable overlay whose data-only lower is the
// store's object directory, and check that file content is served through the
// data plane and that the upper takes writes.
func TestMountMetadataOnlyConsumerOverlayE2E(t *testing.T) {
	storeRoot, cleanup, err := devtest.VerityRoot()
	if errors.Is(err, devtest.ErrNoVerity) {
		t.Skipf("verity-capable store root unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("VerityRoot: %v", err)
	}
	t.Cleanup(cleanup)

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	objects, err := eng.ObjectsDir()
	if err != nil {
		t.Fatalf("ObjectsDir: %v", err)
	}
	if want := filepath.Join(storeRoot, "objects", "fsverity-sha256-12"); objects != want {
		t.Fatalf("ObjectsDir = %q, want %q", objects, want)
	}

	meta := filepath.Join(storeRoot, "meta")
	merged := filepath.Join(storeRoot, "merged")
	upper := filepath.Join(storeRoot, "upper")
	work := filepath.Join(storeRoot, "work")
	for _, dir := range []string{meta, merged, upper, work} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
	}

	var metaMounted, overlayMounted bool
	t.Cleanup(func() {
		if overlayMounted {
			if err := eng.Unmount(context.Background(), merged); err != nil {
				t.Errorf("cleanup unmount overlay: %v", err)
			}
		}
		if metaMounted {
			if err := eng.Unmount(context.Background(), meta); err != nil {
				t.Errorf("cleanup unmount metadata: %v", err)
			}
		}
	})

	err = eng.Mount(context.Background(), lay.Ref, meta, WithMetadataOnly())
	if skipMountEnvironment(err) {
		t.Skipf("mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	metaMounted = true

	// The metadata mount alone carries no file data; content is served only
	// through an overlay with the object directory as its data-only lower.
	opts := "lowerdir=" + meta + "::" + objects +
		",upperdir=" + upper + ",workdir=" + work +
		",redirect_dir=on,metacopy=on,verity=require"
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		t.Fatalf("overlay mount: %v", err)
	}
	overlayMounted = true

	data, err := os.ReadFile(filepath.Join(merged, "etc", "config.txt")) // #nosec G304 -- test mount path
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data, []byte("hello from knarr\n")) {
		t.Fatalf("merged file = %q", data)
	}
	if err := os.WriteFile(filepath.Join(merged, "scratch.txt"), []byte("rw"), 0o644); err != nil {
		t.Fatalf("write through overlay upper: %v", err)
	}
}

// TestExpectedFsverity pins the digest-selection rule: annotation presence
// alone decides between the sealed value and the binding's measurement, the
// standard key wins over the fallback key, and a sealing annotation mount
// cannot verify fails closed instead of falling back to the binding.
func TestExpectedFsverity(t *testing.T) {
	standardHex := strings.Repeat("ab", 32)
	fallbackHex := strings.Repeat("cd", 32)
	binding := "sha256:" + strings.Repeat("ef", 32)
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		want        string
		wantSealed  bool
		wantErr     string
	}{
		{name: "unsealed", want: binding},
		{
			name:        "unrelated annotations only",
			annotations: map[string]string{"org.opencontainers.image.title": "x"},
			want:        binding,
		},
		{
			name:        "standard key",
			annotations: map[string]string{ociseal.MergedAnnotation: standardHex},
			want:        "sha256:" + standardHex, wantSealed: true,
		},
		{
			name:        "fallback key",
			annotations: map[string]string{ociseal.FallbackMergedAnnotation: fallbackHex},
			want:        "sha256:" + fallbackHex, wantSealed: true,
		},
		{
			name: "standard key wins over fallback",
			annotations: map[string]string{
				ociseal.MergedAnnotation:         standardHex,
				ociseal.FallbackMergedAnnotation: fallbackHex,
			},
			want: "sha256:" + standardHex, wantSealed: true,
		},
		{
			name:        "malformed value",
			annotations: map[string]string{ociseal.MergedAnnotation: "not-hex"},
			wantErr:     "sealing annotation",
		},
		{
			name:        "unrecognized digest kind",
			annotations: map[string]string{ociseal.MergedAnnotationPrefix + "fsverity-sha512-12": standardHex},
			wantErr:     "unverifiable sealing annotation",
		},
		{
			name: "unrecognized kind beside the standard key",
			annotations: map[string]string{
				ociseal.MergedAnnotation:                              standardHex,
				ociseal.MergedAnnotationPrefix + "fsverity-sha512-12": fallbackHex,
			},
			want: "sha256:" + standardHex, wantSealed: true,
		},
		{
			name: "unrecognized kind beside the fallback key",
			annotations: map[string]string{
				ociseal.FallbackMergedAnnotation:                      fallbackHex,
				ociseal.MergedAnnotationPrefix + "fsverity-sha512-12": standardHex,
			},
			want: "sha256:" + fallbackHex, wantSealed: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, sealed, err := expectedFsverity(tt.annotations, binding)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expectedFsverity: %v", err)
			}
			if got != tt.want || sealed != tt.wantSealed {
				t.Fatalf("= %s sealed=%v, want %s sealed=%v", got, sealed, tt.want, tt.wantSealed)
			}
		})
	}
}

// TestMountSealedIgnoresTamperedBinding: for a sealed image the annotation on
// the digest-verified manifest, not the binding, decides what the mount
// enforces — so a binding whose recorded digest was tampered with still
// mounts, and the content it serves is the sealed content.
func TestMountSealedIgnoresTamperedBinding(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay := buildCanonicalSealLayout(t, hostPlatform(), sealObjectBody)
	pushLayoutImage(t, registry, "registry.example.com/team/sealed:input", lay)
	sealed, err := ociseal.Seal(context.Background(), "registry.example.com/team/sealed@"+lay.Root.String(), ociseal.Options{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ref := "registry.example.com/team/sealed@" + sealed.Sealed.String()

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	tamperBindingDigest(t, octest.BindingPath(storeRoot, sealed.Sealed, hostPlatform()), digest.SHA256.FromString("wrong").String())

	if _, err := exec.LookPath("mount.composefs"); err != nil {
		t.Skip("mount.composefs not found in PATH")
	}
	target := filepath.Join(storeRoot, "sealed-mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	mounted := false
	t.Cleanup(func() {
		if mounted {
			if err := eng.Unmount(context.Background(), target); err != nil {
				t.Errorf("cleanup unmount: %v", err)
			}
		}
	})
	if err := eng.Mount(context.Background(), ref, target); skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	} else if err != nil {
		t.Fatalf("Mount with tampered binding: %v", err)
	}
	mounted = true

	if data, err := os.ReadFile(filepath.Join(target, "usr", "bin", "tool")); err != nil { // #nosec G304 -- test reads the rootfs it mounted
		t.Fatalf("read object file: %v", err)
	} else if string(data) != sealObjectBody {
		t.Fatalf("object file = %q", data)
	}
	if err := eng.Unmount(context.Background(), target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false
}

// TestMountSealedRefusesRepointedBinding: a binding repointed at a different,
// internally consistent object must not mount a sealed image — the measured
// digest cannot match the sealing annotation.
func TestMountSealedRefusesRepointedBinding(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay := buildCanonicalSealLayout(t, hostPlatform(), sealObjectBody)
	pushLayoutImage(t, registry, "registry.example.com/team/sealed:input", lay)
	sealed, err := ociseal.Seal(context.Background(), "registry.example.com/team/sealed@"+lay.Root.String(), ociseal.Options{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ref := "registry.example.com/team/sealed@" + sealed.Sealed.String()

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// A second, unsealed image supplies the different object the binding is
	// repointed at, so the rewritten binding is internally consistent: its
	// descriptor and fs-verity digest really describe a stored object.
	other, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "usr/bin/other", Mode: 0o755, Body: "a different object body larger than the sixty-four byte inline limit\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	if _, err := eng.Pull(context.Background(), other.Ref, WithFromDir(other.Dir)); err != nil {
		t.Fatalf("Pull other: %v", err)
	}
	ob, err := newImageStore(t, storeRoot).Get(other.Root, hostPlatform())
	if err != nil {
		t.Fatalf("Get other binding: %v", err)
	}
	rewriteBinding(t, octest.BindingPath(storeRoot, sealed.Sealed, hostPlatform()), func(rootfs map[string]any) {
		desc, ok := rootfs["descriptor"].(map[string]any)
		if !ok {
			t.Fatalf("binding descriptor has unexpected shape")
		}
		desc["digest"] = ob.Descriptor.Digest.String()
		desc["size"] = ob.Descriptor.Size
		fsv, ok := rootfs["fsverity"].(map[string]any)
		if !ok {
			t.Fatalf("binding fsverity has unexpected shape")
		}
		fsv["digest"] = ob.FsverityDigest
	})

	err = eng.Mount(context.Background(), ref, t.TempDir())
	if err == nil {
		t.Fatal("expected mount to fail")
	}
	if !strings.Contains(err.Error(), "sealing annotation claims") {
		t.Fatalf("error = %v, want a sealing-annotation mismatch", err)
	}
}

// TestMountSealedFallbackIgnoresTamperedBinding: the knarr fallback annotation
// on a non-canonical sealed image is as authoritative at mount as the standard
// key — a tampered binding digest does not stop the mount.
func TestMountSealedFallbackIgnoresTamperedBinding(t *testing.T) {
	storeRoot := sealVerityStore(t)
	registry := octest.NewRegistry()
	t.Cleanup(registry.Close)
	useRemoteDefaultTransport(t, registry.Transport())

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "model", Dir: true},
		octest.File{Name: "model/weights.bin", Mode: 0o644, Body: sealObjectBody},
		octest.File{Name: "model/config.json", Mode: 0o644, Body: "{}"},
	)
	if err != nil {
		t.Fatalf("BuildSingleLayerGzip: %v", err)
	}
	pushLayoutImage(t, registry, "registry.example.com/team/model:input", lay)
	sealed, err := ociseal.Seal(context.Background(), "registry.example.com/team/model@"+lay.Root.String(), ociseal.Options{AllowNonCanonical: true})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ref := "registry.example.com/team/model@" + sealed.Sealed.String()

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	tamperBindingDigest(t, octest.BindingPath(storeRoot, sealed.Sealed, hostPlatform()), digest.SHA256.FromString("wrong").String())

	if _, err := exec.LookPath("mount.composefs"); err != nil {
		t.Skip("mount.composefs not found in PATH")
	}
	target := filepath.Join(storeRoot, "fallback-mnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	mounted := false
	t.Cleanup(func() {
		if mounted {
			if err := eng.Unmount(context.Background(), target); err != nil {
				t.Errorf("cleanup unmount: %v", err)
			}
		}
	})
	if err := eng.Mount(context.Background(), ref, target); skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	} else if err != nil {
		t.Fatalf("Mount with tampered binding: %v", err)
	}
	mounted = true

	if data, err := os.ReadFile(filepath.Join(target, "model", "weights.bin")); err != nil { // #nosec G304 -- test reads the rootfs it mounted
		t.Fatalf("read object file: %v", err)
	} else if string(data) != sealObjectBody {
		t.Fatalf("object file = %q", data)
	}
	if err := eng.Unmount(context.Background(), target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	mounted = false
}

// TestMountMissingMetadataReportsNotPulled: a binding whose stored metadata
// chain is broken -- the config first, then the identity manifest itself --
// fails the mount with the verb's ErrNotFound remedy instead of silently
// mounting on binding trust alone.
func TestMountMissingMetadataReportsNotPulled(t *testing.T) {
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

	lay, err := octest.BuildSingleLayerGzip(t.TempDir(), hostPlatform(),
		octest.File{Name: "etc/config.txt", Mode: 0o644, Body: "hello from knarr\n"},
	)
	if err != nil {
		t.Fatalf("build layout: %v", err)
	}
	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), lay.Ref, WithFromDir(lay.Dir)); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	requireNotPulled := func(what string) {
		t.Helper()
		err := eng.Mount(context.Background(), lay.Ref, t.TempDir())
		if err == nil {
			t.Fatalf("expected mount to fail with the %s blob missing", what)
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want an ErrNotFound match", err)
		}
		if !strings.Contains(err.Error(), "run `knarr pull` first") {
			t.Fatalf("error = %v, want the re-pull remedy", err)
		}
	}

	// Config first, while the manifest is still present: the resolution must
	// verify the whole chain, not stop at the manifest.
	if err := os.Remove(octest.ObjectPath(storeRoot, lay.Config)); err != nil {
		t.Fatalf("remove config blob: %v", err)
	}
	requireNotPulled("config")
	if err := os.Remove(octest.ObjectPath(storeRoot, lay.Root)); err != nil {
		t.Fatalf("remove manifest blob: %v", err)
	}
	requireNotPulled("manifest")
}
