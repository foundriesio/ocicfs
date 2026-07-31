// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package composefs

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"

	erofs "github.com/erofs/go-erofs"
)

// redirectXattr is the overlay xattr mkcomposefs writes on every by-digest
// regular file: the view-relative object name the mount's data plane resolves,
// with a leading slash.
const redirectXattr = "trusted.overlay.redirect"

// ReferencedObjects returns the store objects the composefs image at path
// redirects to, as the sorted, canonical "<2 hex>/<62 hex>" view-relative
// names WriteDump emitted them under. Hardlinked names share an inode and
// collapse to one entry; inline regular files carry no redirect and
// contribute nothing. The trusted.overlay.metacopy xattr, which carries the
// same fs-verity digest, is deliberately not read: mkcomposefs pairs it with
// a redirect on every by-digest file, so redirects alone name every
// referenced object. A redirect that does not canonicalize to the expected
// shape is an error: gc decides deletions from this enumeration, and a name
// it cannot account for must fail the caller rather than vanish from the set.
func ReferencedObjects(ctx context.Context, path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- store object path, verified by caller
	if err != nil {
		return nil, fmt.Errorf("composefs: open image: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Warn("composefs: failed to close image", "path", path, "err", err)
		}
	}()
	img, err := erofs.Open(f)
	if err != nil {
		return nil, fmt.Errorf("composefs: read image %s: %w", path, err)
	}

	set := map[string]struct{}{}
	err = fs.WalkDir(img, ".", func(p string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("composefs: walk image at %s: %w", p, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !de.Type().IsRegular() {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			return fmt.Errorf("composefs: stat %s: %w", p, err)
		}
		st, ok := fi.Sys().(*erofs.Stat)
		if !ok {
			return fmt.Errorf("composefs: no erofs stat for %s", p)
		}
		redirect, ok := st.Xattrs[redirectXattr]
		if !ok {
			return nil
		}
		name, err := canonicalRedirect(redirect)
		if err != nil {
			return fmt.Errorf("composefs: %s: %w", p, err)
		}
		set[name] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// canonicalRedirect strips the leading slash the on-disk xattr carries —
// composefs-info strips it the same way on output — and requires the
// remaining "<2 hex>/<62 hex>" object-name shape.
func canonicalRedirect(v string) (string, error) {
	name := strings.TrimPrefix(v, "/")
	if len(name) != 65 || name[2] != '/' || !isLowerHex(name[:2]) || !isLowerHex(name[3:]) {
		return "", fmt.Errorf("redirect %q is not a sharded object name", v)
	}
	return name, nil
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
