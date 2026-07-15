// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package composefs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// d builds a sha256 digest whose 64-hex encoding is the byte b repeated, e.g.
// d('1') == sha256:1111...1. Handy for readable golden output.
func d(b byte) digest.Digest {
	return digest.NewDigestFromEncoded(digest.SHA256, string(bytes.Repeat([]byte{b}, 64)))
}

func TestWriteDump(t *testing.T) {
	// Deliberately unsorted, and without a "/" record, to exercise both the
	// stable sort and the synthetic-root injection.
	// Regular files carry distinct content and fs-verity digests so the dump's
	// PAYLOAD (content redirect) and DIGEST (fs-verity) columns are exercised
	// independently: a swap or reuse of one for the other would fail this golden.
	entries := []Entry{
		{Path: "/app", Mode: os.ModeDir | 0o755},
		{Path: "/app/run", Mode: 0o644, Size: 5, Digest: d('1'), Verity: d('a')},
		{Path: "/app/link", Mode: os.ModeSymlink | 0o777, Linkname: "run"},
		{Path: "/dev", Mode: os.ModeDir | 0o755},
		{Path: "/dev/null", Mode: os.ModeDevice | os.ModeCharDevice | 0o666, Devmajor: 1, Devminor: 3},
		{Path: "/app/fifo", Mode: os.ModeNamedPipe | 0o644},
		{Path: "/app/cfg", Mode: 0o600, Size: 2, Digest: d('2'), Verity: d('b'), Xattrs: map[string]string{"user.foo": "bar"}},
		{Path: "/app/run2", Hardlink: "/app/run"},
	}
	// /app/run is hardlinked by /app/run2, so its entry carries Nlink 2.
	entries[1].Nlink = 2

	const ones = "1111111111111111111111111111111111111111111111111111111111111111" // /app/run content
	const twos = "2222222222222222222222222222222222222222222222222222222222222222" // /app/cfg content
	const aaas = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // /app/run fs-verity
	const bbbs = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // /app/cfg fs-verity
	want := "/ 0 40755 1 0 0 0 0.000000000 - - -\n" +
		"/app 0 40755 1 0 0 0 0.000000000 - - -\n" +
		"/app/cfg 2 100600 1 0 0 0 0.000000000 " + twos[:2] + "/" + twos[2:] + " - " + bbbs + " user.foo=bar\n" +
		"/app/fifo 0 10644 1 0 0 0 0.000000000 - - -\n" +
		"/app/link 0 120777 1 0 0 0 0.000000000 run - -\n" +
		"/app/run 5 100644 2 0 0 0 0.000000000 " + ones[:2] + "/" + ones[2:] + " - " + aaas + "\n" +
		"/app/run2 0 @120000 - - - - 0.0 /app/run - -\n" +
		"/dev 0 40755 1 0 0 0 0.000000000 - - -\n" +
		"/dev/null 0 20666 1 0 0 259 0.000000000 - - -\n"

	var got bytes.Buffer
	if err := WriteDump(&got, entries); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	if got.String() != want {
		t.Errorf("WriteDump mismatch:\n got:\n%s\nwant:\n%s", got.String(), want)
	}
}

func TestWriteDumpEscaping(t *testing.T) {
	var got bytes.Buffer
	if err := WriteDump(&got, []Entry{
		{Path: "/a b\tc", Mode: 0o644, Digest: d('3'), Verity: d('4')},
	}); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	// The space and tab in the path must be escaped; "/" sorts first.
	wantPrefix := "/ 0 40755 1 0 0 0 0.000000000 - - -\n/a\\x20b\\tc 0 100644 "
	if got := got.String(); len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("escaped path mismatch:\n got: %q\nwant prefix: %q", got, wantPrefix)
	}
}

func TestWriteDumpEscapesDashSymlinkTarget(t *testing.T) {
	var got bytes.Buffer
	if err := WriteDump(&got, []Entry{
		{Path: "/dash", Mode: os.ModeSymlink | 0o777, Linkname: "-"}, // literal "-" target must be escaped
	}); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	if out := got.String(); !strings.Contains(out, "/dash 0 120777 1 0 0 0 0.000000000 \\x2d - -\n") {
		t.Errorf("literal '-' symlink target should be escaped, not read as unset:\n%s", out)
	}
}

// TestWriteDumpRejectsRegularFileWithoutVerity asserts the dump writer fails
// closed on a content object with no fs-verity digest, so an image cannot carry a
// backing file the kernel will not enforce.
func TestWriteDumpRejectsRegularFileWithoutVerity(t *testing.T) {
	err := WriteDump(&bytes.Buffer{}, []Entry{
		{Path: "/f", Mode: 0o644, Size: 1, Digest: d('1')}, // no Verity
	})
	if err == nil {
		t.Fatal("expected error for a regular file without an fs-verity digest")
	}
}

func TestWriteDumpRejectsRelativePath(t *testing.T) {
	err := WriteDump(&bytes.Buffer{}, []Entry{{Path: "rel", Mode: 0o644, Digest: d('4')}})
	if err == nil {
		t.Fatal("expected error for non-absolute path")
	}
}

func TestWriteDumpRejectsRelativeHardlinkTarget(t *testing.T) {
	err := WriteDump(&bytes.Buffer{}, []Entry{{Path: "/link", Hardlink: "target"}})
	if err == nil {
		t.Fatal("expected error for non-absolute hardlink target")
	}
}

func TestWriteImage(t *testing.T) {
	if _, err := exec.LookPath("mkcomposefs"); err != nil {
		t.Skip("mkcomposefs not found in PATH")
	}
	out := filepath.Join(t.TempDir(), "image.cfs")
	entries := []Entry{
		{Path: "/dir", Mode: os.ModeDir | 0o755},
		{Path: "/dir/hello", Mode: 0o644, Size: 11, Digest: d('a'), Verity: d('b')},
		{Path: "/dir/hello2", Hardlink: "/dir/hello"},
	}
	if err := WriteImage(context.Background(), out, entries); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("composefs image is empty")
	}
}
