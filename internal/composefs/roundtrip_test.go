// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package composefs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// TestWriteImageMatchesUpstream proves WriteImage reproduces the composefs tool's
// own output: it builds an EROFS image from a real directory tree two ways — once
// directly with mkcomposefs, and once by parsing that image's composefs-info dump
// back into Entries and re-running it through WriteImage — then asserts the two
// images are byte-identical and measure the same fs-verity digest. Any drift in
// WriteDump's field encoding, ordering, mode/rdev mapping, or the V1 format pin
// would change the bytes and fail here. It needs mkcomposefs and composefs-info on
// PATH and skips otherwise.
func TestWriteImageMatchesUpstream(t *testing.T) {
	mkcomposefs := lookPathOrSkip(t, "mkcomposefs")
	cfsInfo := lookPathOrSkip(t, "composefs-info")

	// A tree with directories, nested files, and a symlink. The regular files are
	// larger than the inline threshold (64 bytes) so mkcomposefs stores them by
	// digest (payload + fs-verity digest) rather than inlining their content, which
	// parseDump does not handle.
	src := filepath.Join(t.TempDir(), "src")
	mustMkdirAll(t, filepath.Join(src, "dir"))
	mustWriteFile(t, filepath.Join(src, "dir", "a.txt"), bytes.Repeat([]byte("a"), 200))
	mustWriteFile(t, filepath.Join(src, "dir", "b.bin"), bytes.Repeat([]byte{0x7f, 0x00, 0x41}, 100))
	mustWriteFile(t, filepath.Join(src, "top"), bytes.Repeat([]byte("top"), 100))
	if err := os.Symlink("dir/a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	// Reference image straight from the directory. --use-epoch zeroes mtimes to
	// match WriteDump (which fixes mtime at 0), the version is pinned to V1 to match
	// WriteImage, and --skip-xattrs keeps the dump free of any default xattrs the
	// host filesystem might attach so the comparison is hermetic.
	out := filepath.Dir(src)
	ref := filepath.Join(out, "ref.cfs")
	runCmd(t, mkcomposefs, "--use-epoch", "--skip-xattrs", "--min-version=1", "--max-version=1", src, ref)

	// Dump it, parse the dump back into Entries, and rebuild via WriteImage.
	entries := parseDump(t, runCmd(t, cfsInfo, "dump", ref))
	ours := filepath.Join(out, "ours.cfs")
	if err := WriteImage(context.Background(), ours, entries); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}

	// Byte-identical images necessarily measure the same fs-verity digest; assert
	// both, since the digest is the value ocicfs records and enforces.
	if refBytes, oursBytes := mustRead(t, ref), mustRead(t, ours); !bytes.Equal(refBytes, oursBytes) {
		t.Fatalf("rebuilt image differs from the reference (%d vs %d bytes)", len(refBytes), len(oursBytes))
	}
	if refV, oursV := measure(t, cfsInfo, ref), measure(t, cfsInfo, ours); refV == "" || refV != oursV {
		t.Fatalf("fs-verity digest mismatch: reference %q, rebuilt %q", refV, oursV)
	}
}

// TestWriteImageNoInlineMatchesUpstream verifies ocicfs's no-inline path is
// byte-compatible with upstream tooling for files small enough that directory-mode
// mkcomposefs inlines them (<=64 bytes). The reference image is built with upstream
// tools ONLY — never WriteImage: a directory-mode build (which inlines the small
// files) is dumped, every regular-file line is rewritten into ocicfs's
// by-digest form (PAYLOAD and DIGEST both the file's fs-verity digest from
// composefs-info measure-file), and mkcomposefs --from-file rebuilds a
// no-inline image from that dump. WriteImage then rebuilds the same dump and
// must reproduce the reference byte-for-byte, with the small file stored by
// digest rather than re-inlined. Needs mkcomposefs and composefs-info on PATH.
func TestWriteImageNoInlineMatchesUpstream(t *testing.T) {
	mkcomposefs := lookPathOrSkip(t, "mkcomposefs")
	cfsInfo := lookPathOrSkip(t, "composefs-info")

	src := filepath.Join(t.TempDir(), "src")
	mustMkdirAll(t, filepath.Join(src, "dir"))
	mustWriteFile(t, filepath.Join(src, "small"), []byte("tiny"))               // 4 bytes: dir-mode inlines it
	mustWriteFile(t, filepath.Join(src, "dir", "cfg"), []byte("k=v\n"))         // 4 bytes: dir-mode inlines it
	mustWriteFile(t, filepath.Join(src, "big"), bytes.Repeat([]byte("x"), 200)) // >64: by-digest either way
	out := filepath.Dir(src)

	// Directory-mode build inlines the small files into this image.
	inlined := filepath.Join(out, "inlined.cfs")
	runCmd(t, mkcomposefs, "--use-epoch", "--skip-xattrs", "--min-version=1", "--max-version=1", src, inlined)
	inlinedDump := runCmd(t, cfsInfo, "dump", inlined)
	if !hasInlinedRegularFile(inlinedDump) {
		t.Fatalf("directory-mode build did not inline a small file, so the test is not exercising the no-inline path:\n%s", inlinedDump)
	}

	// Upstream-only no-inline reference: rewrite every file to ocicfs's by-digest
	// form and rebuild via --from-file. No WriteImage is involved in producing it.
	// Confirm the once-inlined small file is now by-digest.
	noinlineDump := rewriteToByDigest(t, cfsInfo, src, inlinedDump)
	assertByDigest(t, noinlineDump, "/small")
	dumpPath := filepath.Join(out, "noinline.dump")
	mustWriteFile(t, dumpPath, []byte(noinlineDump))
	ref := filepath.Join(out, "ref.cfs")
	runCmd(t, mkcomposefs, "--min-version=1", "--max-version=1", "--from-file", dumpPath, ref)

	// ocicfs side: parse the same no-inline dump into Entries and rebuild via
	// WriteImage; it must reproduce the upstream reference exactly.
	ours := filepath.Join(out, "ours.cfs")
	if err := WriteImage(context.Background(), ours, parseDump(t, noinlineDump)); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}
	if !bytes.Equal(mustRead(t, ref), mustRead(t, ours)) {
		t.Fatal("ocicfs no-inline image differs from the upstream-built no-inline reference")
	}
	if refV, oursV := measure(t, cfsInfo, ref), measure(t, cfsInfo, ours); refV == "" || refV != oursV {
		t.Fatalf("fs-verity digest mismatch: reference %q, ocicfs %q", refV, oursV)
	}
	// The once-small files must be stored by digest in ocicfs's image, not re-inlined.
	if hasInlinedRegularFile(runCmd(t, cfsInfo, "dump", ours)) {
		t.Fatal("ocicfs image inlined a small file; the no-inline path regressed")
	}
}

// TestWriteImageInlineContentRoundTrip proves an inlined regular file survives a
// build-and-dump cycle: WriteImage runs mkcomposefs on a dump carrying inline
// CONTENT, and composefs-info dumps the resulting image back with the same bytes in
// the CONTENT column (payload and digest empty). The content mixes bytes that must
// be escaped — space, newline, tab, backslash — so the escape round-trip is
// exercised too. Needs mkcomposefs and composefs-info on PATH.
func TestWriteImageInlineContentRoundTrip(t *testing.T) {
	lookPathOrSkip(t, "mkcomposefs") // WriteImage invokes it internally
	cfsInfo := lookPathOrSkip(t, "composefs-info")

	const payload = "k=v x\n\t\\end"
	out := filepath.Join(t.TempDir(), "inline.cfs")
	if err := WriteImage(context.Background(), out, []Entry{
		{Path: "/cfg", Mode: 0o644, Content: []byte(payload)},
	}); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}

	if got := inlineContentOf(t, runCmd(t, cfsInfo, "dump", out), "/cfg"); got != payload {
		t.Fatalf("inline content did not survive round-trip: got %q, want %q", got, payload)
	}
}

// inlineContentOf returns the decoded inline bytes of path from a composefs-info
// dump, asserting the line carries inline content (payload and digest are "-").
func inlineContentOf(t *testing.T, dump, path string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(dump, "\n"), "\n") {
		f := strings.Split(line, " ")
		if len(f) < 11 || f[0] != path {
			continue
		}
		if f[8] != "-" || f[10] != "-" {
			t.Fatalf("%s is not inline (payload=%q digest=%q)", path, f[8], f[10])
		}
		if f[9] == "-" {
			t.Fatalf("%s carries no inline content", path)
		}
		return unescapeField(t, f[9])
	}
	t.Fatalf("%s not found in dump:\n%s", path, dump)
	return ""
}

// unescapeField decodes a composefs-dump field — the inverse of the writer's
// escaping for the sequences the dump emits (\\, \n, \r, \t, \xHH).
func unescapeField(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			t.Fatalf("dangling escape in %q", s)
		}
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'x':
			if i+2 >= len(s) {
				t.Fatalf("truncated hex escape in %q", s)
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				t.Fatalf("bad hex escape in %q: %v", s, err)
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			t.Fatalf("unknown escape \\%c in %q", s[i], s)
		}
	}
	return b.String()
}

// parseDump converts a composefs-info dump back into the Entry inventory that
// produced it — the inverse of WriteDump for the node types this test generates
// (directories, by-digest regular files, symlinks). It is deliberately strict:
// inline content, hardlinks, or xattrs (none of which the fixture produces, and
// which it does not parse) fail the test rather than silently round-tripping
// something other than what was dumped.
func parseDump(t *testing.T, dump string) []Entry {
	t.Helper()
	var entries []Entry
	for _, line := range strings.Split(strings.TrimRight(dump, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, " ")
		if len(f) != 11 {
			t.Fatalf("dump line has %d fields, want exactly 11 (no xattrs expected): %q", len(f), line)
		}
		mode := parseDumpMode(t, f[2])
		e := Entry{
			Path: unescaped(t, f[0]),
			Mode: mode,
			UID:  atoi(t, f[4]),
			GID:  atoi(t, f[5]),
			Size: int64(atoi(t, f[1])),
		}
		payload, content, dig := f[8], f[9], f[10]
		switch {
		case mode&os.ModeSymlink != 0:
			e.Linkname = unescaped(t, payload)
		case mode&os.ModeType == 0:
			if content != "-" {
				t.Fatalf("regular file %q carries inline content; the fixture must exceed the inline threshold", e.Path)
			}
			// payload is "<2>/<rest>" of the fs-verity digest and seeds Digest, so
			// the entry carries the content key WriteDump requires; the DIGEST column,
			// also the fs-verity digest, seeds Verity, from which WriteDump derives
			// the emitted payload. In these fs-verity-keyed dumps the two coincide.
			e.Digest = digestFromPayload(t, payload)
			e.Verity = digest.NewDigestFromEncoded(digest.SHA256, dig)
		case mode&os.ModeDevice != 0:
			rdev := uint64(atoi(t, f[6])) // #nosec G115 -- small test rdev
			e.Devmajor = int64(unix.Major(rdev))
			e.Devminor = int64(unix.Minor(rdev))
		}
		entries = append(entries, e)
	}
	return entries
}

// parseDumpMode parses a composefs-dump st_mode octal field into an os.FileMode —
// the inverse of unixMode.
func parseDumpMode(t *testing.T, s string) os.FileMode {
	t.Helper()
	if strings.HasPrefix(s, "@") {
		t.Fatalf("hardlink entry in dump (%q); the fixture should not produce one", s)
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		t.Fatalf("mode %q: %v", s, err)
	}
	st := uint32(v)
	m := os.FileMode(st & 0o777)
	switch st & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= os.ModeDir
	case unix.S_IFLNK:
		m |= os.ModeSymlink
	case unix.S_IFCHR:
		m |= os.ModeCharDevice | os.ModeDevice
	case unix.S_IFBLK:
		m |= os.ModeDevice
	case unix.S_IFIFO:
		m |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= os.ModeSocket
	}
	if st&unix.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if st&unix.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if st&unix.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	return m
}

// digestFromPayload turns a "<2>/<rest>" by-digest payload back into the full
// sha256 digest it encodes.
func digestFromPayload(t *testing.T, payload string) digest.Digest {
	t.Helper()
	hex := strings.Replace(payload, "/", "", 1)
	d := digest.NewDigestFromEncoded(digest.SHA256, hex)
	if err := d.Validate(); err != nil {
		t.Fatalf("payload %q is not a sha256 by-digest path: %v", payload, err)
	}
	return d
}

// unescaped returns a dump field unchanged, asserting it carries no escape
// sequences — the fixture uses only plain ASCII names, so any backslash means the
// fixture drifted into territory this helper does not decode.
func unescaped(t *testing.T, s string) string {
	t.Helper()
	if strings.Contains(s, `\`) {
		t.Fatalf("dump field %q is escaped; keep the fixture names plain", s)
	}
	return s
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("not an integer: %q: %v", s, err)
	}
	return n
}

// measure returns the fs-verity digest composefs-info computes for path. It works
// on any filesystem: composefs-info falls back to a userspace computation when the
// file has no kernel fs-verity, so no privileged mount is needed.
func measure(t *testing.T, cfsInfo, path string) string {
	t.Helper()
	return strings.TrimSpace(runCmd(t, cfsInfo, "measure-file", path))
}

// hasInlinedRegularFile reports whether any dump line carries inline content in the
// CONTENT column (the field after PAYLOAD). Only regular files can; directories,
// symlinks, and device nodes always leave it "-".
func hasInlinedRegularFile(dump string) bool {
	for _, line := range strings.Split(strings.TrimRight(dump, "\n"), "\n") {
		if line == "" {
			continue
		}
		if f := strings.Split(line, " "); len(f) >= 11 && f[9] != "-" {
			return true
		}
	}
	return false
}

// rewriteToByDigest turns a composefs-info dump into ocicfs's no-inline by-digest
// form using only upstream tools. Every regular-file line — inlined or already
// by-digest — is rewritten so PAYLOAD is the "<2>/<rest>" redirect for the file's
// fs-verity digest (from composefs-info measure-file), CONTENT is "-", and DIGEST
// is that same fs-verity digest — the fs-verity-keyed form WriteImage emits.
// Directories, symlinks, and other non-regular lines pass through unchanged.
func rewriteToByDigest(t *testing.T, cfsInfo, srcDir, dump string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(dump, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, " ")
		if len(f) != 11 {
			t.Fatalf("dump line has %d fields, want exactly 11 (no xattrs expected): %q", len(f), line)
		}
		src := filepath.Join(srcDir, strings.TrimPrefix(unescaped(t, f[0]), "/"))
		if fi, err := os.Lstat(src); err == nil && fi.Mode().IsRegular() {
			fsv := measure(t, cfsInfo, src) // fs-verity -> PAYLOAD and DIGEST
			if len(fsv) != 2*32 {
				t.Fatalf("measure-file %q returned %q, want a 64-hex sha256 digest", src, fsv)
			}
			f[8] = fsv[:2] + "/" + fsv[2:]
			f[9] = "-"
			f[10] = fsv
		}
		b.WriteString(strings.Join(f, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// assertByDigest fails unless dump holds a by-digest line for path: CONTENT is
// "-" and the PAYLOAD redirect reconstructs the DIGEST column (the file's
// fs-verity digest) — the fs-verity-keyed form ocicfs shares with upstream.
func assertByDigest(t *testing.T, dump, path string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(dump, "\n"), "\n") {
		f := strings.Split(line, " ")
		if len(f) != 11 || f[0] != path {
			continue
		}
		if f[9] != "-" {
			t.Fatalf("%s is inlined (CONTENT=%q), want by-digest", path, f[9])
		}
		if payload := strings.Replace(f[8], "/", "", 1); payload != f[10] {
			t.Fatalf("%s PAYLOAD %q does not reconstruct DIGEST %q; expected the fs-verity redirect", path, f[8], f[10])
		}
		return
	}
	t.Fatalf("%s not found in dump:\n%s", path, dump)
}

func lookPathOrSkip(t *testing.T, bin string) string {
	t.Helper()
	p, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("%s not found in PATH", bin)
	}
	return p
}

func runCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	var stdout, stderr strings.Builder
	cmd := exec.Command(name, args...) // #nosec G204 -- test helper, PATH-resolved binary and fixed args
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v (stderr=%s)", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String()
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWriteImageMatchesRustMkcomposefs proves the dump WriteImage feeds to the C
// mkcomposefs yields the same EROFS bytes under composefs-rs's mkcomposefs
// (cfsctl's multi-call entry, the drop-in for the C binary). The fixture spans
// every column WriteDump emits: directories with xattrs and non-zero mtime, a
// by-digest regular file, an inlined one, a symlink, a hardlink with its name
// count, a device node, and a fifo. Needs mkcomposefs and cfsctl on PATH.
func TestWriteImageMatchesRustMkcomposefs(t *testing.T) {
	lookPathOrSkip(t, "mkcomposefs") // WriteImage invokes it internally
	cfsctl := lookPathOrSkip(t, "cfsctl")

	entries := []Entry{
		{Path: "/", Mode: os.ModeDir | 0o755, Mtime: 1700000000},
		{Path: "/dir", Mode: os.ModeDir | 0o750, UID: 1000, GID: 1000, Mtime: 1700000000, MtimeNsec: 500, Xattrs: map[string]string{"user.k": "v"}},
		{Path: "/dir/data", Mode: 0o644, Size: 4096, Digest: d('a'), Verity: d('b'), Nlink: 2, Mtime: 1700000001},
		{Path: "/dir/data.link", Hardlink: "/dir/data"},
		{Path: "/dir/inline", Mode: 0o600, Content: []byte("k=v\n"), Mtime: 1700000002},
		{Path: "/dir/link", Mode: os.ModeSymlink | 0o777, Linkname: "data"},
		{Path: "/dev-null", Mode: os.ModeDevice | os.ModeCharDevice | 0o666, Devmajor: 1, Devminor: 3},
		{Path: "/fifo", Mode: os.ModeNamedPipe | 0o600},
		{Path: "/suid", Mode: os.ModeSetuid | 0o755, Size: 100, Digest: d('c'), Verity: d('e')},
	}

	out := t.TempDir()
	ours := filepath.Join(out, "ours.cfs")
	if err := WriteImage(context.Background(), ours, entries); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}

	var dump bytes.Buffer
	if err := WriteDump(&dump, entries); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	dumpPath := filepath.Join(out, "image.dump")
	mustWriteFile(t, dumpPath, dump.Bytes())
	rust := filepath.Join(out, "rust.cfs")
	runCmd(t, cfsctl, "mkcomposefs", "--min-version=1", "--max-version=1", "--from-file", dumpPath, rust)

	if oursBytes, rustBytes := mustRead(t, ours), mustRead(t, rust); !bytes.Equal(oursBytes, rustBytes) {
		t.Fatalf("the Rust mkcomposefs built a different image from the same dump (%d vs %d bytes)", len(oursBytes), len(rustBytes))
	}
}
