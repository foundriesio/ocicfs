// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

// Package composefs generates a composefs/EROFS image from a logical inventory.
// It owns only the on-disk format: it serialises the inventory as a
// composefs-dump(5) stream and runs mkcomposefs to turn it into an EROFS image.
package composefs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// InlineContentMax is the largest regular-file size, in bytes, stored inline in
// the CONTENT column instead of by digest in the CAS.
const InlineContentMax = 64

// Entry is one node of the composefs image — one line of the composefs-dump(5)
// stream WriteDump emits, whose fixed fields are
//
//	PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST [xattrs...]
//
// The fields below map onto those columns (see the per-field comments). WriteDump
// fixes the MTIME column at 0 (reproducible builds). A regular file is emitted one
// of two ways. With Content set, its bytes are inlined into the EROFS (the CONTENT
// column) and it carries no PAYLOAD or DIGEST. Otherwise it is stored by digest:
// its bytes live in a separate fs-verity-sealed CAS object that PAYLOAD redirects
// into and DIGEST validates, which is what keeps per-file integrity and content
// dedup. An entry with Hardlink set is another
// name for an earlier entry: it carries only Path and Hardlink, and mkcomposefs
// gives both names one shared inode whose metadata and content come from the
// target entry. mkcomposefs recomputes directory link counts but takes a leaf's
// st_nlink from the NLINK column, so a multiply-linked entry must carry its
// name count in Nlink for the mounted inode to report it.
//
// Both PAYLOAD and DIGEST are the fs-verity value (Verity): PAYLOAD redirects
// through the object store's fs-verity view and DIGEST is what the kernel
// validates the backing object against, matching upstream composefs, which keys
// its store by fs-verity digest. Digest, the object's content sha256, stays the
// store's content-addressed key but is not part of the emitted line.
type Entry struct {
	Path     string            // PATH: absolute path of the node in the image
	Mode     os.FileMode       // MODE: Go file-type + permission bits, mapped to the st_mode column
	UID, GID int               // UID, GID: owner ids
	Xattrs   map[string]string // trailing KEY=VALUE xattr fields
	Linkname string            // PAYLOAD, for symlinks: the link target
	Hardlink string            // PAYLOAD, for hardlinks: absolute path of the earlier entry this name shares an inode with; when set every other field except Path is ignored
	Content  []byte            // CONTENT, for regular files: inline file bytes; when non-nil the file is inlined and carries no Digest or Verity
	Digest   digest.Digest     // content digest of the backing CAS object (the store's content-addressed key); required for a by-digest regular file but not emitted in the dump line
	Verity   digest.Digest     // PAYLOAD and DIGEST, for by-digest regular files: the fs-verity digest, emitted both as the "<2>/<rest>" view redirect and as the value the kernel validates
	Size     int64             // SIZE: file size (ignored for directories)
	Devmajor int64             // RDEV (major): device number for device nodes
	Devminor int64             // RDEV (minor): device number for device nodes
	Nlink    int               // NLINK: the inode's name count; 0 is written as 1
}

// WriteImage streams a composefs dump for entries to a temp file beside out and
// runs mkcomposefs to produce the EROFS image at out. The dump is written
// straight to the temp file rather than buffered whole in memory.
func WriteImage(ctx context.Context, out string, entries []Entry) error {
	if out == "" {
		return errors.New("composefs: empty output path")
	}

	dumpFile, err := os.CreateTemp(filepath.Dir(out), ".cfsdump-*") // #nosec G304 -- store-owned output dir
	if err != nil {
		return fmt.Errorf("composefs: create dump temp: %w", err)
	}
	dumpPath := dumpFile.Name()
	defer func() { _ = os.Remove(dumpPath) }()

	bw := bufio.NewWriter(dumpFile)
	if err := WriteDump(bw, entries); err != nil {
		_ = dumpFile.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		_ = dumpFile.Close()
		return fmt.Errorf("composefs: flush dump: %w", err)
	}
	if err := dumpFile.Close(); err != nil {
		return fmt.Errorf("composefs: close dump: %w", err)
	}

	// Args are discrete (no shell) and the binary name is fixed; the paths are
	// store-owned. The version is pinned to 1 (min and max) so the EROFS is always
	// emitted in the stable V1 format rather than letting mkcomposefs pick the
	// lowest version that happens to fit.
	cmd := exec.CommandContext(ctx, "mkcomposefs", "--min-version=1", "--max-version=1", "--from-file", dumpPath, out) // #nosec G204 -- fixed binary, store-owned discrete args
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("composefs: mkcomposefs: %w (stderr=%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// WriteDump serialises entries in composefs-dump(5) format to w. Entries are
// sorted lexicographically by path so each parent directory precedes its
// children (the format's invariant), and a synthetic root "/" is prepended when
// the inventory does not carry one.
func WriteDump(w io.Writer, entries []Entry) error {
	work := make([]Entry, 0, len(entries)+1)
	hasRoot := false
	for _, e := range entries {
		if e.Path == "/" {
			hasRoot = true
		}
		work = append(work, e)
	}
	if !hasRoot {
		work = append(work, Entry{Path: "/", Mode: os.ModeDir | 0o755})
	}
	sort.SliceStable(work, func(i, j int) bool { return work[i].Path < work[j].Path })

	for i := range work {
		if err := writeDumpLine(w, &work[i]); err != nil {
			return fmt.Errorf("composefs: dump entry %q: %w", work[i].Path, err)
		}
	}
	return nil
}

// writeDumpLine emits one composefs-dump(5) record:
//
//	PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST [xattrs...]
//
// knarr does not track per-file mtime, so MTIME is fixed at zero (reproducible).
func writeDumpLine(w io.Writer, e *Entry) error {
	if e.Path == "" || e.Path[0] != '/' {
		return fmt.Errorf("path must be absolute, got %q", e.Path)
	}

	if e.Hardlink != "" {
		if e.Hardlink[0] != '/' {
			return fmt.Errorf("hardlink target must be absolute, got %q", e.Hardlink)
		}
		// A hardlink line carries only the path and its target: the '@' prefix
		// on the mode column marks it (the mode value after it is ignored, as
		// are the stat columns, written as placeholders) and PAYLOAD names the
		// target entry, whose line defines the shared inode's metadata,
		// content, and xattrs.
		_, err := fmt.Fprintf(w, "%s 0 @120000 - - - - 0.0 %s - -\n",
			escapeDumpField(e.Path), escapeDumpField(e.Hardlink))
		return err
	}

	size := e.Size
	if e.Mode&os.ModeDir != 0 {
		size = 0
	}
	if e.Content != nil && e.Mode&os.ModeType != 0 {
		return fmt.Errorf("entry %q has inline content but is not a regular file", e.Path)
	}
	if e.Mode&os.ModeType == 0 && e.Content != nil {
		// An inlined regular file's size is exactly its inline byte count; the
		// dump parser rejects a CONTENT length that disagrees with SIZE.
		size = int64(len(e.Content))
	}
	rdev := uint64(0)
	if e.Mode&os.ModeDevice != 0 {
		rdev = unix.Mkdev(uint32(e.Devmajor), uint32(e.Devminor)) // #nosec G115 -- device major/minor fit in uint32
	}

	nlink := e.Nlink
	if nlink < 1 {
		nlink = 1
	}
	if _, err := fmt.Fprintf(w, "%s %d %o %d %d %d %d 0.000000000 ",
		escapeDumpField(e.Path), size, unixMode(e.Mode), nlink, e.UID, e.GID, rdev); err != nil {
		return err
	}

	switch {
	case e.Mode&os.ModeSymlink != 0:
		if e.Linkname == "" {
			return errors.New("symlink missing Linkname")
		}
		if _, err := fmt.Fprintf(w, "%s - -", escapeOptionalField(e.Linkname)); err != nil {
			return err
		}
	case e.Mode&os.ModeType == 0 && e.Content != nil:
		// Inlined regular file: PAYLOAD = "-", CONTENT = the escaped inline bytes,
		// DIGEST = "-". The bytes live in the EROFS itself, so there is no backing
		// CAS object and no fs-verity digest to carry.
		if e.Digest != "" || e.Verity != "" {
			return fmt.Errorf("regular file %q has both inline content and a digest", e.Path)
		}
		if len(e.Content) > InlineContentMax {
			return fmt.Errorf("regular file %q inline content is %d bytes, over the %d-byte limit", e.Path, len(e.Content), InlineContentMax)
		}
		content := "-"
		if len(e.Content) > 0 {
			content = escapeOptionalField(string(e.Content))
		}
		if _, err := fmt.Fprintf(w, "- %s -", content); err != nil {
			return err
		}
	case e.Mode&os.ModeType == 0:
		// By-digest regular file: PAYLOAD = "<2>/<rest>" of the fs-verity digest (the
		// redirect into the object store's fs-verity view), CONTENT = "-", DIGEST = the
		// same fs-verity digest the kernel validates the backing object against. Digest,
		// the content-addressed store key, must be present so a backing object exists,
		// but it is not emitted in the line.
		if e.Digest == "" {
			return errors.New("regular file missing Digest")
		}
		if e.Verity == "" {
			return fmt.Errorf("regular file %q missing fs-verity Verity digest", e.Path)
		}
		enc := e.Verity.Encoded()
		if len(enc) < 2 {
			return fmt.Errorf("verity digest too short: %s", e.Verity)
		}
		if _, err := fmt.Fprintf(w, "%s/%s - %s", enc[:2], enc[2:], e.Verity.Encoded()); err != nil {
			return err
		}
	default:
		// Directories, devices, fifos, sockets carry no payload.
		if _, err := io.WriteString(w, "- - -"); err != nil {
			return err
		}
	}

	if len(e.Xattrs) > 0 {
		keys := make([]string, 0, len(e.Xattrs))
		for k := range e.Xattrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, " %s=%s", escapeXattrKey(k), escapeDumpField(e.Xattrs[k])); err != nil {
				return err
			}
		}
	}

	_, err := io.WriteString(w, "\n")
	return err
}

// unixMode maps a Go os.FileMode to the traditional st_mode encoding
// (type bits | permission bits) that composefs-dump(5) expects.
func unixMode(m os.FileMode) uint32 {
	out := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		out |= unix.S_ISUID
	}
	if m&os.ModeSetgid != 0 {
		out |= unix.S_ISGID
	}
	if m&os.ModeSticky != 0 {
		out |= unix.S_ISVTX
	}
	switch {
	case m&os.ModeDir != 0:
		out |= unix.S_IFDIR
	case m&os.ModeSymlink != 0:
		out |= unix.S_IFLNK
	case m&os.ModeCharDevice != 0:
		out |= unix.S_IFCHR
	case m&os.ModeDevice != 0:
		out |= unix.S_IFBLK
	case m&os.ModeNamedPipe != 0:
		out |= unix.S_IFIFO
	case m&os.ModeSocket != 0:
		out |= unix.S_IFSOCK
	default:
		out |= unix.S_IFREG
	}
	return out
}

// escapeDumpField applies the composefs-dump(5) escape rules: spaces,
// non-printables, and backslashes are escaped, with short forms for the common
// control characters.
func escapeDumpField(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == ' ':
			b.WriteString(`\x20`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// escapeOptionalField escapes a value bound for an optional dump field (a
// symlink's payload, or any field whose unset form is a bare "-"). A literal "-"
// is encoded so it is not read back as the unset sentinel; everything else
// follows the ordinary escape rules.
func escapeOptionalField(s string) string {
	if s == "-" {
		return `\x2d`
	}
	return escapeDumpField(s)
}

// escapeXattrKey is escapeDumpField plus '=' escaping, since '=' separates an
// xattr key from its value.
func escapeXattrKey(k string) string {
	return strings.ReplaceAll(escapeDumpField(k), "=", `\x3d`)
}
