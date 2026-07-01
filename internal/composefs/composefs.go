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

// Entry is one node of the composefs image — one line of the composefs-dump(5)
// stream WriteDump emits, whose fixed fields are
//
//	PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST [xattrs...]
//
// The fields below map onto those columns (see the per-field comments). WriteDump
// fixes the columns Entry does not model: NLINK is always 1 (mkcomposefs recomputes
// directory link counts), MTIME is always 0 (reproducible builds), and CONTENT is
// always "-". knarr never inlines file content into the image: every regular file
// is stored by digest, so its bytes live in a separate fs-verity-sealed CAS object
// rather than in the EROFS, which is what keeps per-file integrity and content
// dedup. Hardlinks are resolved to regular files referencing the target's backing
// object before they reach here.
//
// PAYLOAD and DIGEST are distinct in knarr: PAYLOAD redirects into the
// content-sha256-keyed CAS (derived from Digest) while DIGEST is the fs-verity
// value (Verity). Upstream composefs keys its store by fs-verity digest, so for it
// the two coincide; knarr's content-addressed store makes them different values.
type Entry struct {
	Path     string            // PATH: absolute path of the node in the image
	Mode     os.FileMode       // MODE: Go file-type + permission bits, mapped to the st_mode column
	UID, GID int               // UID, GID: owner ids
	Xattrs   map[string]string // trailing KEY=VALUE xattr fields
	Linkname string            // PAYLOAD, for symlinks: the link target
	Digest   digest.Digest     // PAYLOAD, for regular files: content digest of the backing CAS object, emitted as the "<2>/<rest>" redirect
	Verity   digest.Digest     // DIGEST: fs-verity digest the kernel validates the backing object against; required for regular files
	Size     int64             // SIZE: file size (ignored for directories)
	Devmajor int64             // RDEV (major): device number for device nodes
	Devminor int64             // RDEV (minor): device number for device nodes
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
// knarr does not track per-file mtime or link counts, so MTIME is fixed at zero
// (reproducible) and NLINK at 1 (mkcomposefs recomputes directory link counts).
func writeDumpLine(w io.Writer, e *Entry) error {
	if e.Path == "" || e.Path[0] != '/' {
		return fmt.Errorf("path must be absolute, got %q", e.Path)
	}

	size := e.Size
	if e.Mode&os.ModeDir != 0 {
		size = 0
	}
	rdev := uint64(0)
	if e.Mode&os.ModeDevice != 0 {
		rdev = unix.Mkdev(uint32(e.Devmajor), uint32(e.Devminor)) // #nosec G115 -- device major/minor fit in uint32
	}

	if _, err := fmt.Fprintf(w, "%s %d %o 1 %d %d %d 0.000000000 ",
		escapeDumpField(e.Path), size, unixMode(e.Mode), e.UID, e.GID, rdev); err != nil {
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
	case e.Mode&os.ModeType == 0:
		// Regular file: PAYLOAD = "<2>/<rest>" of the content digest (the redirect
		// into the CAS object store), CONTENT = "-", DIGEST = the fs-verity digest
		// the kernel validates the backing object against. Both are required: a
		// content object with no fs-verity digest is rejected, since an image must
		// not carry a backing file the kernel will not enforce.
		if e.Digest == "" {
			return errors.New("regular file missing Digest")
		}
		if e.Verity == "" {
			return fmt.Errorf("regular file %q missing fs-verity Verity digest", e.Path)
		}
		enc := e.Digest.Encoded()
		if len(enc) < 2 {
			return fmt.Errorf("digest too short: %s", e.Digest)
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
