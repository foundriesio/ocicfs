#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# storage-demo.sh — runnable companion to docs/storage-walkthrough.md.
#
# Plays both sides of knarr's trust model on one machine using only the upstream
# mkcomposefs and composefs-info tools:
#   [publisher] builds a content-addressed, no-inline composefs image from a sample
#               tree and seals its fs-verity digest;
#   [device]    rebuilds the image from the same dump + objects and verifies the
#               measured digest matches the seal.
#
# It deliberately uses only upstream tools and no knarr code, so it validates the
# composefs/fs-verity format and trust-model guarantees that knarr's implementation
# builds on, independently of knarr itself. It is a demonstration of the substrate,
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
printf 'hello from knarr' >tree/hello       # 16 B  -> mkcomposefs inlines
printf 'k=v\n' >tree/etc/app.conf           # 4 B   -> mkcomposefs inlines
head -c 200 /dev/random >tree/bin/tool      # 200 B -> by-digest (random content)
find tree -mindepth 1 -printf '%y %8s  %p\n' | sort -k3

echo
echo "== [publisher] structure from mkcomposefs (small files inlined) =="
mkcomposefs --use-epoch --skip-xattrs --min-version=1 --max-version=1 tree inlined.cfs
composefs-info dump inlined.cfs

echo
echo "== [publisher] rewrite to a content-addressed, no-inline dump (+ build object store) =="
mkdir -p objects/sha256
to_knarr() {
	while IFS= read -r line; do
		# shellcheck disable=SC2086 # dump fields are space-separated and unescaped here
		set -- $line # PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST
		# This demo maps the dump PATH straight to a source file, so it handles only
		# plain names. Refuse an escaped PATH rather than silently leaving its file
		# inlined, which would contradict the no-inline image this builds.
		case $1 in
		*\\*)
			echo "storage-demo: dump PATH '$1' is escaped; this demo handles only plain names" >&2
			exit 1
			;;
		esac
		f="tree$1"
		if [ -f "$f" ] && [ ! -L "$f" ]; then
			csum=$(sha256sum "$f" | cut -d' ' -f1)                    # content digest -> PAYLOAD
			install -D -m0644 "$f" "objects/sha256/${csum:0:2}/${csum:2}" # store the object
			fsv=$(composefs-info measure-file "$f")                   # fs-verity digest -> DIGEST
			printf '%s %s %s %s %s %s %s %s %s/%s - %s\n' \
				"$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "${csum:0:2}" "${csum:2}" "$fsv"
		else
			printf '%s\n' "$line"
		fi
	done
}
composefs-info dump inlined.cfs | to_knarr >image.dump
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
missing="$(composefs-info missing-objects --basedir=objects/sha256 device.cfs)"
echo "missing from the object store: ${missing:-(none)}"
