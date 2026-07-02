#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# knarr smoke test: exercise the local image lifecycle against a real image.
#
# Drives the built knarr binary through pull --from-dir -> ls -> mount ->
# unmount, then through mount --metadata-only with the overlay a writable
# consumer assembles over it (objects/sha256 as the data-only lowerdir,
# verity=require), and finally remove, so the currently implemented
# functionality can be checked by hand in one command. It needs a privileged,
# fs-verity-capable environment with mkcomposefs and mount.composefs on PATH
# -- i.e. the dev container. Either open the dev container and run the target
# inside it:
#
#   ./devshell.sh                # opens a shell in the dev container; then: make smoke
#
# or run it one-shot against a container built from the Containerfile:
#
#   docker run --rm --privileged -v "$PWD":/work knarr-dev make smoke
#
# Env:
#   SMOKE_IMAGE   registry image to pull (default docker.io/library/busybox:latest)
#   SMOKE_LAYOUT  OCI layout directory; cached across runs, so a repeat run needs no
#                 network (pre-populate it to run fully offline)
set -euo pipefail

IMAGE="${SMOKE_IMAGE:-docker.io/library/busybox:latest}"
LAYOUT="${SMOKE_LAYOUT:-/tmp/knarr-smoke-layout}"
STORE=""
MNT=""
OVL=""

cleanup() {
	set +e
	if [ -n "$OVL" ]; then
		bin/knarr --root "$STORE" unmount "$OVL/merged" 2>/dev/null
		bin/knarr --root "$STORE" unmount "$OVL/meta" 2>/dev/null
		rm -rf "$OVL"
	fi
	[ -n "$MNT" ] && bin/knarr --root "$STORE" unmount "$MNT" 2>/dev/null
	[ -n "$STORE" ] && rm -rf "$STORE"
	[ -n "$MNT" ] && rm -rf "$MNT"
}
trap cleanup EXIT

STORE="$(mktemp -d /var/lib/knarr/smoke-XXXXXX)"
MNT="$(mktemp -d)"
KN=(bin/knarr --root "$STORE")

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }

[ -x bin/knarr ] || make build

if [ ! -f "$LAYOUT/index.json" ]; then
	echo "== crane pull $IMAGE -> $LAYOUT =="
	crane pull --format=oci "$IMAGE" "$LAYOUT"
fi
ref="smoke@$(jq -r '.manifests[0].digest' "$LAYOUT/index.json")"

echo "== pull =="
"${KN[@]}" pull --from-dir "$LAYOUT" "$ref"

echo "== ls (expect the pulled image) =="
listed="$("${KN[@]}" ls)"
printf '%s\n' "$listed"
printf '%s\n' "$listed" | grep -qF "$ref" || fail "ls did not list $ref after pull"

echo "== mount =="
"${KN[@]}" mount "$ref" "$MNT"
echo "== mounted rootfs (expect non-empty, read-only) =="
ls -la "$MNT"
[ -n "$(ls -A "$MNT")" ] || fail "mounted rootfs is empty"
if touch "$MNT/.smoke-rw-probe" 2>/dev/null; then fail "mounted rootfs is writable"; fi

echo "== unmount (expect the mountpoint emptied) =="
"${KN[@]}" unmount "$MNT"
[ -z "$(ls -A "$MNT")" ] || fail "mountpoint not empty after unmount"

echo "== mount --metadata-only + consumer-assembled overlay =="
# The upper and work directories must not live on an overlayfs, so keep the
# whole leg beside the store on the verity-capable filesystem.
OVL="$(mktemp -d /var/lib/knarr/smoke-ovl-XXXXXX)"
mkdir "$OVL/meta" "$OVL/merged" "$OVL/upper" "$OVL/work"
"${KN[@]}" mount --metadata-only "$ref" "$OVL/meta"
mount -t overlay overlay -o \
	"lowerdir=$OVL/meta::$STORE/objects/sha256,upperdir=$OVL/upper,workdir=$OVL/work,redirect_dir=on,metacopy=on,verity=require" \
	"$OVL/merged"

echo "== merged rootfs (expect file data served from the object store) =="
f="$(find "$OVL/merged" -xdev -type f -size +0c -print -quit)"
[ -n "$f" ] || fail "merged rootfs has no file content"
cat "$f" >/dev/null || fail "reading $f through the verity overlay failed"
touch "$OVL/merged/.smoke-rw-probe" || fail "merged overlay is not writable"

"${KN[@]}" unmount "$OVL/merged"
"${KN[@]}" unmount "$OVL/meta"
[ -z "$(ls -A "$OVL/meta")" ] || fail "metadata mountpoint not empty after unmount"

echo "== remove =="
"${KN[@]}" remove "$ref"
echo "== ls (expect the image gone) =="
listed="$("${KN[@]}" ls)"
printf '%s\n' "$listed"
if printf '%s\n' "$listed" | grep -qF "$ref"; then fail "ls still lists $ref after remove"; fi

echo "SMOKE OK: pull -> ls -> mount -> unmount -> metadata-only overlay -> remove (checks passed)"
