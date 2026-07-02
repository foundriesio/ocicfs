#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# knarr smoke test: pull a real image from a local OCI layout, mount it, then unmount.
#
# Drives the built knarr binary through the pull --from-dir -> mount -> unmount happy
# path against a real image, so the currently implemented functionality can be checked
# by hand in one command. It needs a privileged, fs-verity-capable environment with
# mkcomposefs and mount.composefs on PATH -- i.e. the dev container. Either open the
# dev container and run the target inside it:
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

cleanup() {
	set +e
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
echo "== mount =="
"${KN[@]}" mount "$ref" "$MNT"
echo "== mounted rootfs (expect non-empty, read-only) =="
ls -la "$MNT"
[ -n "$(ls -A "$MNT")" ] || fail "mounted rootfs is empty"
if touch "$MNT/.smoke-rw-probe" 2>/dev/null; then fail "mounted rootfs is writable"; fi

echo "== unmount (expect the mountpoint emptied) =="
"${KN[@]}" unmount "$MNT"
[ -z "$(ls -A "$MNT")" ] || fail "mountpoint not empty after unmount"

echo "SMOKE OK: pull --from-dir -> mount -> unmount (checks passed)"
