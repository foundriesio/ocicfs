// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package knarr

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ggcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/foundriesio/knarr/internal/devtest"
	"github.com/foundriesio/knarr/internal/octest"
)

// TestMountMatchesFlattenedImageE2E is a differential test of the layer merge:
// the same multi-layer image is flattened by an independent implementation
// (go-containerregistry's mutate.Extract, the engine behind `crane export`)
// and materialized+mounted by knarr, and the two resulting trees must agree on
// every path, type, permission and setuid/setgid/sticky bit, ownership, size,
// content, symlink target, device number, xattr, and hardlink grouping.
// Excluded on purpose: mtimes (the canonical construction rewrites the root's
// and /run's, so the trees legitimately differ there), the canonical tree
// transforms themselves (the fixture stays transform-neutral — no /run
// children, allowlisted xattrs only — since the rootfs unit tests pin them),
// and the two merge cases the reference implementation itself gets wrong —
// opaque-directory markers (it treats ".wh..wh..opq" as a file whiteout) and
// whiting out one name of a hardlink group (it emits a dangling link) — which
// stay covered by the rootfs unit tests.
func TestMountMatchesFlattenedImageE2E(t *testing.T) {
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

	layer1 := diffTar(t,
		diffEnt{name: "./", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "bin/tool", typ: tar.TypeReg, mode: 0o4755, body: "tool-v1"},
		diffEnt{name: "bin/tool2", typ: tar.TypeLink, link: "bin/tool"},
		diffEnt{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "etc/conf", typ: tar.TypeReg, mode: 0o644, body: "conf-v1", uid: 1000, gid: 2000},
		diffEnt{
			name: "etc/secret", typ: tar.TypeReg, mode: 0o600, body: "secret",
			// A well-formed VFS_CAP_REVISION_2 blob (effective, CAP_NET_BIND_SERVICE):
			// the kernel validates security.capability on read, so an arbitrary
			// string would EINVAL through the mount.
			pax: map[string]string{
				"SCHILY.xattr.security.capability": "\x01\x00\x00\x02\x00\x04\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
			},
		},
		diffEnt{name: "etc/rel", typ: tar.TypeSymlink, mode: 0o777, link: "conf"},
		diffEnt{name: "share/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "share/doc/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "share/doc/readme", typ: tar.TypeReg, mode: 0o644, body: "read me"},
		diffEnt{name: "dev/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "dev/null2", typ: tar.TypeChar, mode: 0o666, devmajor: 1, devminor: 3},
		diffEnt{name: "srv/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "srv/pipe", typ: tar.TypeFifo, mode: 0o644},
		diffEnt{name: "junk/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "junk/data", typ: tar.TypeReg, mode: 0o644, body: "junk"},
		diffEnt{name: "old/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "old/gone", typ: tar.TypeReg, mode: 0o644, body: "bye"},
		diffEnt{name: "old/sub/", typ: tar.TypeDir, mode: 0o755},
		diffEnt{name: "old/sub/f", typ: tar.TypeReg, mode: 0o644, body: "x"},
	)
	layer2 := diffTar(t,
		diffEnt{name: ".wh.old", typ: tar.TypeReg, mode: 0o000},
		diffEnt{name: "etc/conf", typ: tar.TypeReg, mode: 0o640, body: "conf-v2", uid: 1000, gid: 2000},
		diffEnt{name: "etc/conf2", typ: tar.TypeLink, link: "etc/conf"},
		diffEnt{name: "share/doc/readme2", typ: tar.TypeLink, link: "share/doc/readme"},
		diffEnt{name: "junk", typ: tar.TypeSymlink, mode: 0o777, link: "share"},
		diffEnt{name: "newdir/", typ: tar.TypeDir, mode: 0o700},
		diffEnt{name: "newdir/file", typ: tar.TypeReg, mode: 0o644, body: "new"},
	)
	img := craftedImage(t, layer1, layer2)

	want := flattenedEntries(t, img)

	reg := octest.NewRegistry()
	t.Cleanup(reg.Close)
	useRemoteDefaultTransport(t, reg.Transport())
	root := pushRegistryImage(t, "registry.example.com", "team/diff", img, reg.Transport())
	ref := "registry.example.com/team/diff@" + root.String()

	eng, err := New(Config{Root: storeRoot})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Pull(context.Background(), ref); devtest.IsVerityUnsupported(err) {
		t.Skipf("fs-verity unsupported on the test filesystem: %v", err)
	} else if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	target := filepath.Join(storeRoot, "diffmnt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	err = eng.Mount(context.Background(), ref, target)
	if skipMountEnvironment(err) {
		t.Skipf("composefs mount environment unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Unmount(context.Background(), target); err != nil {
			t.Errorf("cleanup unmount: %v", err)
		}
	})

	compareMountToFlattened(t, target, want)
}

// diffEnt is one crafted tar entry of the differential test image.
type diffEnt struct {
	name     string
	typ      byte
	mode     int64
	body     string
	link     string
	uid, gid int
	devmajor int64
	devminor int64
	pax      map[string]string
}

// refEntry is the reference state of one path, parsed from the flattened tar.
type refEntry struct {
	typ      byte
	mode     int64
	uid, gid int
	size     int64
	body     digest.Digest
	link     string
	devmajor int64
	devminor int64
	xattrs   map[string]string
}

func diffTar(t *testing.T, entries ...diffEnt) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:       e.name,
			Typeflag:   e.typ,
			Mode:       e.mode,
			Linkname:   e.link,
			Uid:        e.uid,
			Gid:        e.gid,
			Devmajor:   e.devmajor,
			Devminor:   e.devminor,
			PAXRecords: e.pax,
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar write %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// craftedImage assembles the layer tars into an image whose config declares the
// host platform, so knarr's platform selection accepts it.
func craftedImage(t *testing.T, layers ...[]byte) ggcrv1.Image {
	t.Helper()
	img := empty.Image
	for i, l := range layers {
		data := l
		lyr, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		})
		if err != nil {
			t.Fatalf("layer %d: %v", i, err)
		}
		img, err = mutate.AppendLayers(img, lyr)
		if err != nil {
			t.Fatalf("append layer %d: %v", i, err)
		}
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	cfg.OS, cfg.Architecture = hostPlatform().OS, hostPlatform().Architecture
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("set platform: %v", err)
	}
	return img
}

// flattenedEntries merges the image with the reference implementation and
// parses the flattened tar into per-path reference state. Paths are keyed
// root-relative; the root directory entry itself is dropped.
func flattenedEntries(t *testing.T, img ggcrv1.Image) map[string]refEntry {
	t.Helper()
	want := make(map[string]refEntry)
	tr := tar.NewReader(mutate.Extract(img))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read flattened tar: %v", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || name == "/" {
			continue
		}
		e := refEntry{
			typ:      hdr.Typeflag,
			mode:     hdr.Mode & 0o7777,
			uid:      hdr.Uid,
			gid:      hdr.Gid,
			size:     hdr.Size,
			link:     hdr.Linkname,
			devmajor: hdr.Devmajor,
			devminor: hdr.Devminor,
		}
		for k, v := range hdr.PAXRecords {
			if after, ok := cutXattrKey(k); ok {
				if e.xattrs == nil {
					e.xattrs = make(map[string]string)
				}
				e.xattrs[after] = v
			}
		}
		if hdr.Typeflag == tar.TypeLink {
			e.link = filepath.Clean(hdr.Linkname)
		}
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read flattened %s: %v", name, err)
			}
			e.body = digest.FromBytes(body)
		}
		if _, dup := want[name]; dup {
			t.Fatalf("flattened tar names %s twice", name)
		}
		want[name] = e
	}
	if len(want) == 0 {
		t.Fatal("flattened tar is empty")
	}
	return want
}

func cutXattrKey(paxKey string) (string, bool) {
	const prefix = "SCHILY.xattr."
	if len(paxKey) <= len(prefix) || paxKey[:len(prefix)] != prefix {
		return "", false
	}
	return paxKey[len(prefix):], true
}

// compareMountToFlattened walks the mounted tree and checks every path against
// the reference state, then verifies both directions saw the same path set and
// that hardlink groups share one inode with the right link count.
func compareMountToFlattened(t *testing.T, target string, want map[string]refEntry) {
	t.Helper()
	seen := make(map[string]bool, len(want))
	err := filepath.WalkDir(target, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(target, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		ref, ok := want[rel]
		if !ok {
			t.Errorf("mount has %s, flattened image does not", rel)
			return nil
		}
		seen[rel] = true
		compareEntry(t, p, rel, ref, want)
		return nil
	})
	if err != nil {
		t.Fatalf("walk mount: %v", err)
	}
	for rel := range want {
		if !seen[rel] {
			t.Errorf("flattened image has %s, mount does not", rel)
		}
	}

	// Hardlink names must share their target's inode, and the inode's link
	// count must equal its name count.
	groups := make(map[string][]string) // payload path -> all names
	for rel, ref := range want {
		if ref.typ == tar.TypeLink {
			groups[ref.link] = append(groups[ref.link], rel)
		}
	}
	for targetRel, links := range groups {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(target, targetRel), &st); err != nil {
			t.Errorf("lstat %s: %v", targetRel, err)
			continue
		}
		// st.Nlink is uint64 on amd64 but uint32 on arm64; unify for comparison.
		if got, wantN := uint64(st.Nlink), uint64(len(links)+1); got != wantN {
			t.Errorf("%s nlink = %d, want %d", targetRel, got, wantN)
		}
		for _, link := range links {
			var lst unix.Stat_t
			if err := unix.Lstat(filepath.Join(target, link), &lst); err != nil {
				t.Errorf("lstat %s: %v", link, err)
				continue
			}
			if lst.Ino != st.Ino {
				t.Errorf("%s inode = %d, want %s's inode %d", link, lst.Ino, targetRel, st.Ino)
			}
		}
	}
}

func compareEntry(t *testing.T, abs, rel string, ref refEntry, want map[string]refEntry) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(abs, &st); err != nil {
		t.Errorf("lstat %s: %v", rel, err)
		return
	}

	// A hardlink name must present as its target: same type, metadata, and
	// content (the shared-inode identity is checked at group level).
	if ref.typ == tar.TypeLink {
		targetRef, ok := want[ref.link]
		if !ok {
			t.Errorf("%s links to %s, which the flattened image lacks", rel, ref.link)
			return
		}
		ref = targetRef
	}

	wantFmt, checkPerm := map[byte]uint32{
		tar.TypeReg:     unix.S_IFREG,
		tar.TypeDir:     unix.S_IFDIR,
		tar.TypeSymlink: unix.S_IFLNK,
		tar.TypeChar:    unix.S_IFCHR,
		tar.TypeBlock:   unix.S_IFBLK,
		tar.TypeFifo:    unix.S_IFIFO,
	}[ref.typ], ref.typ != tar.TypeSymlink
	if wantFmt == 0 {
		t.Errorf("%s: unhandled reference type %q", rel, ref.typ)
		return
	}
	if st.Mode&unix.S_IFMT != wantFmt {
		t.Errorf("%s type = %o, want %o", rel, st.Mode&unix.S_IFMT, wantFmt)
		return
	}
	if checkPerm && uint64(st.Mode&0o7777) != uint64(ref.mode) { // #nosec G115 -- masked to 12 bits
		t.Errorf("%s mode = %o, want %o", rel, st.Mode&0o7777, ref.mode)
	}
	if int(st.Uid) != ref.uid || int(st.Gid) != ref.gid {
		t.Errorf("%s owner = %d:%d, want %d:%d", rel, st.Uid, st.Gid, ref.uid, ref.gid)
	}

	switch ref.typ {
	case tar.TypeReg:
		if st.Size != ref.size {
			t.Errorf("%s size = %d, want %d", rel, st.Size, ref.size)
		}
		data, err := os.ReadFile(abs) // #nosec G304 -- test walks its own mount
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
		} else if got := digest.FromBytes(data); got != ref.body {
			t.Errorf("%s content digest = %s, want %s", rel, got, ref.body)
		}
	case tar.TypeSymlink:
		got, err := os.Readlink(abs)
		if err != nil {
			t.Errorf("readlink %s: %v", rel, err)
		} else if got != ref.link {
			t.Errorf("%s -> %q, want %q", rel, got, ref.link)
		}
	case tar.TypeChar, tar.TypeBlock:
		if got, wantDev := st.Rdev, unix.Mkdev(uint32(ref.devmajor), uint32(ref.devminor)); got != wantDev { // #nosec G115 -- test device numbers fit
			t.Errorf("%s rdev = %d, want %d", rel, got, wantDev)
		}
	}

	if ref.typ == tar.TypeReg || ref.typ == tar.TypeDir {
		for k, v := range ref.xattrs {
			buf := make([]byte, 256)
			n, err := unix.Lgetxattr(abs, k, buf)
			if err != nil {
				t.Errorf("%s xattr %s: %v", rel, k, err)
				continue
			}
			if string(buf[:n]) != v {
				t.Errorf("%s xattr %s = %q, want %q", rel, k, buf[:n], v)
			}
		}
	}
}
