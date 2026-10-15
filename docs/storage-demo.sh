#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# storage-demo.sh — runnable companion to docs/storage-walkthrough.md.
#
# Plays both sides of ocicfs's trust model on one machine using only the
# upstream mkcomposefs and composefs-info tools:
#   [publisher] builds a composefs image from a sample tree — files over 64 bytes
#               stored by content sha256 with an fs-verity-keyed hardlink view and
#               redirects keyed by fs-verity, files of 64 bytes or fewer inlined
#               into the EROFS as mkcomposefs does — and seals its fs-verity digest;
#   [device]    rebuilds the image from the same dump + objects and verifies the
#               measured digest matches the seal.
#
# It deliberately uses only upstream tools and no ocicfs code, so it validates the
# composefs/fs-verity format and trust-model guarantees that ocicfs's implementation
# builds on, independently of ocicfs itself. It is a demonstration of the substrate,
# not a general-purpose image builder. Run it inside the dev container (./devshell.sh); no privilege is needed —
# composefs-info measures fs-verity in userspace.

set -euo pipefail

for tool in mkcomposefs composefs-info; do
	command -v "$tool" >/dev/null || {
		echo "storage-demo: '$tool' not found; run inside the dev container (./devshell.sh)" >&2
		exit 1
	}
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"

echo "== [publisher] sample tree =="
mkdir -p tree/etc tree/bin
printf 'hello from ocicfs' >tree/hello       # 16 B  -> mkcomposefs inlines
printf 'k=v\n' >tree/etc/app.conf           # 4 B   -> mkcomposefs inlines
head -c 200 /dev/random >tree/bin/tool      # 200 B -> by-digest (random content)
find tree -mindepth 1 -printf '%y %8s  %p\n' | sort -k3

echo
echo "== [publisher] structure from mkcomposefs (small files inlined) =="
mkcomposefs --use-epoch --skip-xattrs --min-version=1 --max-version=1 tree inlined.cfs
composefs-info dump inlined.cfs

echo
echo "== [publisher] externalize files over 64 bytes (+ build object store and fs-verity view) =="
mkdir -p objects/sha256 objects/fsverity-sha256-12
to_ocicfs() {
	while IFS= read -r line; do
		# shellcheck disable=SC2086 # dump fields are space-separated and unescaped here
		set -- $line # PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST
		# This demo maps the dump PATH straight to a source file, so it handles only
		# plain names. Refuse an escaped PATH rather than mishandle it.
		case $1 in
		*\\*)
			echo "storage-demo: dump PATH '$1' is escaped; this demo handles only plain names" >&2
			exit 1
			;;
		esac
		f="tree$1"
		# ocicfs inlines files of <=64 bytes exactly as mkcomposefs already did, so
		# those dump lines pass through untouched; only larger regular files become
		# by-digest objects. A regular file's SIZE column ($2) is its byte length.
		if [ -f "$f" ] && [ ! -L "$f" ] && [ "$2" -gt 64 ]; then
			csum=$(sha256sum "$f" | cut -d' ' -f1)                    # content digest -> CAS name
			install -D -m0644 "$f" "objects/sha256/${csum:0:2}/${csum:2}" # store the object
			fsv=$(composefs-info measure-file "$f")                   # fs-verity digest -> PAYLOAD + DIGEST
			mkdir -p "objects/fsverity-sha256-12/${fsv:0:2}"
			ln -f "objects/sha256/${csum:0:2}/${csum:2}" "objects/fsverity-sha256-12/${fsv:0:2}/${fsv:2}" # view hardlink
			printf '%s %s %s %s %s %s %s %s %s/%s - %s\n' \
				"$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "${fsv:0:2}" "${fsv:2}" "$fsv"
		else
			printf '%s\n' "$line"
		fi
	done
}
composefs-info dump inlined.cfs | to_ocicfs >image.dump
cat image.dump

echo
echo "== [publisher] build canonical image and seal its fs-verity =="
mkcomposefs --min-version=1 --max-version=1 --from-file image.dump publisher.cfs
seal=$(composefs-info measure-file publisher.cfs)
echo "sealed fs-verity digest: $seal"

echo
echo "== [device] rebuild from the delivered dump + objects, then verify =="
mkcomposefs --min-version=1 --max-version=1 --from-file image.dump device.cfs
got=$(composefs-info measure-file device.cfs)
echo "device rebuilt fs-verity     = $got"
if [ "$got" = "$seal" ]; then
	echo "VERIFY OK: the device image matches the sealed digest"
else
	echo "VERIFY FAILED: digests differ" >&2
	exit 1
fi

echo
echo "== [device] delta view =="
echo "objects this image references:"
composefs-info objects device.cfs
missing="$(composefs-info missing-objects --basedir=objects/fsverity-sha256-12 device.cfs)"
echo "missing from the object store: ${missing:-(none)}"
