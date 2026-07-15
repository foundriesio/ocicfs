#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# knarr smoke test: exercise the local image lifecycle against a real image.
#
# Drives the built knarr binary through the full lifecycle -- pull -> ls ->
# inspect -> mount -> unmount, then mount --metadata-only with the overlay a
# writable consumer assembles over it (objects/sha256 as the data-only
# lowerdir, verity=require), and finally remove -- three times: for an image
# sideloaded with pull --from-dir, for the same image pulled digest-pinned from
# a loopback HTTP registry (crane registry serve), and for the same image
# pulled and addressed by tag (checked to resolve to the same pinned digest),
# so the currently implemented functionality can be checked by hand in one
# command. It needs a privileged,
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
#                 network (pre-populate it to run fully offline; the registry leg is
#                 loopback-local and needs no network either)
set -euo pipefail

IMAGE="${SMOKE_IMAGE:-docker.io/library/busybox:latest}"
LAYOUT="${SMOKE_LAYOUT:-/tmp/knarr-smoke-layout}"
STORE=""
MNT=""
OVL=""
REGPID=""

cleanup() {
	set +e
	if [ -n "$OVL" ]; then
		bin/knarr --root "$STORE" unmount "$OVL/merged" 2>/dev/null
		bin/knarr --root "$STORE" unmount "$OVL/meta" 2>/dev/null
		rm -rf "$OVL"
	fi
	[ -n "$MNT" ] && bin/knarr --root "$STORE" unmount "$MNT" 2>/dev/null
	[ -n "$REGPID" ] && kill "$REGPID" 2>/dev/null
	[ -n "$STORE" ] && rm -rf "$STORE"
	[ -n "$MNT" ] && rm -rf "$MNT"
}
trap cleanup EXIT

STORE="$(mktemp -d /var/lib/knarr/smoke-XXXXXX)"
MNT="$(mktemp -d)"
KN=(bin/knarr --root "$STORE")

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }

# run_lifecycle drives every implemented verb for one image: $1 is the
# reference the store ends up holding (digest-pinned or a tag), $2 the root
# digest it must resolve to, and the remaining arguments are the pull
# invocation that puts it there.
run_lifecycle() {
	local ref="$1" digest="$2"
	shift 2

	echo "== pull ($*) =="
	"${KN[@]}" "$@"

	echo "== ls (expect the pulled image) =="
	local listed
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed"
	printf '%s\n' "$listed" | grep -qF "$ref" || fail "ls did not list $ref after pull"

	echo "== inspect (text, then json checked against the pulled digest) =="
	"${KN[@]}" inspect "$ref"
	local inspected
	inspected="$("${KN[@]}" inspect --format json "$ref")"
	[ "$(jq -r .root <<<"$inspected")" = "$digest" ] || fail "inspect root is not the pulled digest"
	jq -e '.composefs.digest | startswith("sha256:")' <<<"$inspected" >/dev/null || fail "inspect reports no composefs digest"
	jq -e '.fsverity != ""' <<<"$inspected" >/dev/null || fail "inspect reports no fsverity digest"

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
	local f
	f="$(find "$OVL/merged" -xdev -type f -size +0c -print -quit)"
	[ -n "$f" ] || fail "merged rootfs has no file content"
	cat "$f" >/dev/null || fail "reading $f through the verity overlay failed"
	touch "$OVL/merged/.smoke-rw-probe" || fail "merged overlay is not writable"

	"${KN[@]}" unmount "$OVL/merged"
	"${KN[@]}" unmount "$OVL/meta"
	[ -z "$(ls -A "$OVL/meta")" ] || fail "metadata mountpoint not empty after unmount"
	rm -rf "$OVL"
	OVL=""

	echo "== remove =="
	"${KN[@]}" remove "$ref"
	echo "== ls (expect the image gone) =="
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed"
	if printf '%s\n' "$listed" | grep -qF "$ref"; then fail "ls still lists $ref after remove"; fi
}

[ -x bin/knarr ] || make build

if [ ! -f "$LAYOUT/index.json" ]; then
	echo "== crane pull $IMAGE -> $LAYOUT =="
	crane pull --format=oci "$IMAGE" "$LAYOUT"
fi
digest="$(jq -r '.manifests[0].digest' "$LAYOUT/index.json")"

echo "==== lifecycle 1: image sideloaded from a local directory ===="
run_lifecycle "smoke@$digest" "$digest" pull --from-dir "$LAYOUT" "smoke@$digest"

echo "==== lifecycle 2: image pulled from a loopback HTTP registry ===="
REGLOG="$(mktemp)"
crane registry serve --address 127.0.0.1:0 >"$REGLOG" 2>&1 &
REGPID=$!
port=""
for _ in $(seq 50); do
	port="$(sed -n 's/.*serving on port \([0-9][0-9]*\).*/\1/p' "$REGLOG")"
	[ -n "$port" ] && break
	sleep 0.1
done
[ -n "$port" ] || { cat "$REGLOG" >&2; fail "crane registry serve reported no port"; }
crane push "$LAYOUT" "127.0.0.1:$port/smoke"
run_lifecycle "127.0.0.1:$port/smoke@$digest" "$digest" pull "127.0.0.1:$port/smoke@$digest"

echo "==== lifecycle 3: image pulled and addressed by tag ===="
run_lifecycle "127.0.0.1:$port/smoke:latest" "$digest" pull "127.0.0.1:$port/smoke:latest"

echo "SMOKE OK: pull -> ls -> inspect -> mount -> unmount -> metadata-only overlay -> remove, from --from-dir and from a registry, digest-pinned and by tag (checks passed)"
