#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# knarr smoke test: exercise the image lifecycle and sealing against a real
# image and a real registry.
#
# Phase 0 builds a purpose-built image offline with umoci -- object, inline,
# and empty files, a symlink, /run content the canonical transform drops, and
# a second layer with a whiteout and an override, deliberately hardlink-free
# -- pushes it to the local registry, seals it as a plain manifest target
# (the pushing seal reads from the local layout via --from-dir, after a
# dry-run parity check against the registry source; the busybox seal leg
# below covers the registry-sourced push), runs seal-crosscheck.sh on it
# expecting the exact three-way digest match, and drives the sealed image
# through the full verb lifecycle.
#
# The rest exercises a real image: the full lifecycle -- pull -> ls ->
# inspect -> mount -> unmount, then mount --metadata-only with the overlay a
# writable consumer assembles over it (the objects/fsverity-sha256-12 view as
# lowerdir, verity=require), then hold (a plain remove must decline naming
# the holder) -> release -> remove -- three times: for an image
# sideloaded with pull --from-dir, for the same image pulled digest-pinned from
# a loopback HTTP registry (zot), and for the same image
# pulled and addressed by tag (checked to resolve to the same pinned digest),
# so the currently implemented functionality can be checked by hand in one
# command. A final differential leg diffs the mounted rootfs against the same
# image flattened by crane export, so the layer merge is checked against an
# independent implementation on a real image. A sealing leg then runs
# `knarr seal` against the registry for the host platform of the multi-arch
# image -- dry-run parity, the annotation check on the raw pushed manifest,
# and the sealed image's own full lifecycle, whose pull verifies the seal
# device-side. The registry legs run against a local zot
# instance -- a real OCI registry that enforces the distribution spec on
# manifest PUTs, unlike an in-process test registry. It needs a privileged,
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
#   SMOKE_IMAGE     registry image to pull (default docker.io/library/busybox:latest)
#   SMOKE_LAYOUT    OCI layout directory; cached across runs, so a repeat run needs no
#                   network (pre-populate it to run fully offline; the registry leg is
#                   loopback-local and needs no network either)
#   SMOKE_REG_PORT  loopback port for the smoke run's own zot instance
#                   (default 5001; the dev container's persistent registry
#                   owns 5000)
set -euo pipefail

IMAGE="${SMOKE_IMAGE:-docker.io/library/busybox:latest}"
LAYOUT="${SMOKE_LAYOUT:-/tmp/knarr-smoke-layout}"
STORE=""
MNT=""
OVL=""
REGPID=""
REGDIR=""
IMGDIR=""
EXPORTDIR=""

cleanup() {
	set +e
	if [ -n "$OVL" ]; then
		bin/knarr --root "$STORE" unmount "$OVL/merged" 2>/dev/null
		bin/knarr --root "$STORE" unmount "$OVL/meta" 2>/dev/null
		rm -rf "$OVL"
	fi
	[ -n "$MNT" ] && bin/knarr --root "$STORE" unmount "$MNT" 2>/dev/null
	[ -n "$REGPID" ] && kill "$REGPID" 2>/dev/null
	[ -n "$REGDIR" ] && rm -rf "$REGDIR"
	[ -n "$IMGDIR" ] && rm -rf "$IMGDIR"
	[ -n "$STORE" ] && rm -rf "$STORE"
	[ -n "$MNT" ] && rm -rf "$MNT"
	[ -n "$EXPORTDIR" ] && rm -rf "$EXPORTDIR"
}
trap cleanup EXIT

STORE="$(mktemp -d /var/lib/knarr/smoke-XXXXXX)"
MNT="$(mktemp -d)"
KN=(bin/knarr --root "$STORE")

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }

# host_child prints the digest of the manifest an image index offers for the
# host platform -- the identity knarr keys the image by. $1 is the index JSON.
host_child() {
	jq -r --arg arch "$(go env GOARCH)" \
		'[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch)][0].digest' \
		<<<"$1"
}

# run_lifecycle drives every implemented verb for one image: $1 is the
# reference the store ends up holding (digest-pinned or a tag), $2 the image
# identity it must resolve to (the platform manifest), $3 the digest the
# reference itself lands on (the same as $2 unless the reference names an
# index), and the remaining arguments are the pull invocation that puts it
# there.
run_lifecycle() {
	local ref="$1" digest="$2" pulled="$3"
	shift 3

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
	[ "$(jq -r .root <<<"$inspected")" = "$digest" ] || fail "inspect root is not the image identity"

	echo "== ref record (expect identity keyed, provenance preserved) =="
	# One image is in the store at this point, so the single ref record under
	# refs/ is this leg's. The on-disk layout is a documented contract.
	local record
	record="$(cat "$STORE"/refs/sha256/*/*.json)"
	printf '%s\n' "$record"
	[ "$(jq -r .identity.digest <<<"$record")" = "$digest" ] || fail "ref record identity is not the image identity"
	[ "$(jq -r .resolved.digest <<<"$record")" = "$pulled" ] || fail "ref record resolved is not the pulled digest"
	jq -e '.composefs.digest | startswith("sha256:")' <<<"$inspected" >/dev/null || fail "inspect reports no composefs digest"
	jq -e '.fsverity != ""' <<<"$inspected" >/dev/null || fail "inspect reports no fsverity digest"
	jq -e '.rootfs_size > 0' <<<"$inspected" >/dev/null || fail "inspect reports no merged rootfs size"

	echo "== inspect --layers (table, then json layer identity checks) =="
	"${KN[@]}" inspect --layers "$ref"
	local layered
	layered="$("${KN[@]}" inspect --layers --format json "$ref")"
	[ "$(jq -r '.layers | length' <<<"$layered")" -ge 1 ] || fail "inspect --layers reports no layers"
	jq -e '[.layers[].diffID | startswith("sha256:")] | all' <<<"$layered" >/dev/null || fail "a layer diffID is malformed"
	[ "$(jq -r '.layers[0].chainID' <<<"$layered")" = "$(jq -r '.diff_ids[0]' <<<"$layered")" ] \
		|| fail "first layer chainID is not the first diff_id"
	jq -e '[.layers[].size > 0] | all' <<<"$layered" >/dev/null || fail "a layer reports no archive size"

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
		"lowerdir=$OVL/meta::$STORE/objects/fsverity-sha256-12,upperdir=$OVL/upper,workdir=$OVL/work,redirect_dir=on,metacopy=on,verity=require" \
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

	echo "== hold (expect a plain remove to decline, naming the holder) =="
	"${KN[@]}" hold "$ref"
	local declined
	if declined="$("${KN[@]}" remove "$ref" 2>&1)"; then
		fail "remove succeeded while $ref was held"
	fi
	printf '%s\n' "$declined"
	printf '%s\n' "$declined" | grep -qF "held by cli" || fail "declined remove did not name the holder"
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed" | grep -qF "$ref" || fail "ls does not list $ref after the declined remove"

	echo "== release, then remove =="
	"${KN[@]}" release "$ref"
	"${KN[@]}" remove "$ref"
	echo "== ls (expect the image gone) =="
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed"
	if printf '%s\n' "$listed" | grep -qF "$ref"; then fail "ls still lists $ref after remove"; fi
}

[ -x bin/knarr ] || make build

# Every phase talks to the run's own empty zot instance rather than the dev
# container's persistent one on 5000, so every assertion runs against
# hermetic per-run registry state untouched by manual testing or previous
# runs.
port="${SMOKE_REG_PORT:-5001}"
REGDIR="$(mktemp -d)"
cat >"$REGDIR/config.json" <<EOF
{
  "distSpecVersion": "1.1.1",
  "storage": { "rootDirectory": "$REGDIR/storage" },
  "http": { "address": "127.0.0.1", "port": "$port" }
}
EOF
zot serve "$REGDIR/config.json" >"$REGDIR/zot.log" 2>&1 &
REGPID=$!
for _ in $(seq 100); do
	curl -fsS "http://127.0.0.1:$port/v2/" >/dev/null 2>&1 && break
	sleep 0.1
done
curl -fsS "http://127.0.0.1:$port/v2/" >/dev/null 2>&1 \
	|| { cat "$REGDIR/zot.log" >&2; fail "zot did not become ready on port $port"; }

echo "==== phase 0: purpose-built image -> seal -> cross-check -> lifecycle ===="
# An offline, hardlink-free image covering the content classes and merge
# semantics the busybox legs cannot pin down: an object-sized and an
# inline-sized file, an empty file, a symlink, an empty directory, /run
# content the canonical transform drops, and a second layer carrying a
# whiteout and an override. Hardlink-free so the cross-check below must
# reach the exact three-way digest match, not its fallback.
echo "== build with umoci and push =="
IMGDIR="$(mktemp -d /var/lib/knarr/smoke-img-XXXXXX)"
umoci init --layout "$IMGDIR/layout"
umoci new --image "$IMGDIR/layout:custom"
umoci unpack --image "$IMGDIR/layout:custom" "$IMGDIR/b1" >/dev/null 2>&1
r="$IMGDIR/b1/rootfs"
mkdir -p "$r/usr/bin" "$r/usr/share/empty" "$r/etc" "$r/run"
seq 1 40 >"$r/usr/bin/app" && chmod 0755 "$r/usr/bin/app"
printf 'inline config\n' >"$r/etc/config"
: >"$r/etc/empty"
ln -s ../../etc/config "$r/usr/share/link"
printf 'scratch\n' >"$r/run/junk"
printf 'doomed\n' >"$r/etc/doomed"
umoci repack --image "$IMGDIR/layout:custom" "$IMGDIR/b1" >/dev/null 2>&1
umoci unpack --image "$IMGDIR/layout:custom" "$IMGDIR/b2" >/dev/null 2>&1
rm "$IMGDIR/b2/rootfs/etc/doomed"
printf 'overridden\n' >"$IMGDIR/b2/rootfs/etc/config"
umoci repack --image "$IMGDIR/layout:custom" "$IMGDIR/b2" >/dev/null 2>&1
umoci config --image "$IMGDIR/layout:custom" --os linux --architecture "$(go env GOARCH)" >/dev/null 2>&1
umoci gc --layout "$IMGDIR/layout" >/dev/null 2>&1
cdigest="$(jq -r '.manifests[0].digest' "$IMGDIR/layout/index.json")"
crane push "$IMGDIR/layout" "127.0.0.1:$port/custom"
cref="127.0.0.1:$port/custom@$cdigest"

echo "== seal (plain manifest target: dry-run parity, then --from-dir push) =="
cdry="$("${KN[@]}" seal --dry-run --format json "$cref" | jq -r .sealed)"
[[ "$cdry" == sha256:* ]] || fail "custom seal --dry-run reported no sealed digest"
cfromdir="$("${KN[@]}" seal --dry-run --from-dir "$IMGDIR/layout" --format json "$cref" | jq -r .sealed)"
[ "$cfromdir" = "$cdry" ] || fail "custom seal --from-dir dry-run digest differs from the registry dry-run"
csealed="$("${KN[@]}" seal --from-dir "$IMGDIR/layout" --format json "$cref" | jq -r .sealed)"
[ "$csealed" = "$cdry" ] || fail "custom sealed digest differs between dry-run and --from-dir push"
cman="$(crane manifest "127.0.0.1:$port/custom@$csealed")"
jq -e '.annotations["composefs.merged.erofs.v1.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$cman" >/dev/null || fail "custom sealed manifest lacks a well-formed merged fsverity annotation"
jq -e '.config.annotations["composefs.config.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$cman" >/dev/null || fail "custom sealed manifest lacks a well-formed config fsverity annotation"

echo "== seal-crosscheck (expect the exact three-way match) =="
ccheck="$(CROSSCHECK_REGISTRY="127.0.0.1:$port" scripts/seal-crosscheck.sh "$cref")"
printf '%s\n' "$ccheck" | grep -E "knarr:|cfsctl:|mkcomposefs:|RESULT"
printf '%s\n' "$ccheck" | grep -qF "RESULT: all three digests MATCH" \
	|| fail "seal-crosscheck did not reach the exact three-way match"

echo "==== lifecycle 0: the sealed purpose-built image ===="
# The purpose-built image is a plain manifest, so it is its own identity.
run_lifecycle "127.0.0.1:$port/custom@$csealed" "$csealed" "$csealed" pull "127.0.0.1:$port/custom@$csealed"
rm -rf "$IMGDIR"
IMGDIR=""

if [ ! -f "$LAYOUT/index.json" ]; then
	echo "== crane pull $IMAGE -> $LAYOUT =="
	crane pull --format=oci "$IMAGE" "$LAYOUT"
fi
digest="$(jq -r '.manifests[0].digest' "$LAYOUT/index.json")"
# The layout pins a multi-arch index, so the identity these legs must land on is
# the child manifest it offers for the host platform.
identity="$(host_child "$(cat "$LAYOUT/blobs/sha256/${digest#sha256:}")")"
[[ "$identity" == sha256:* ]] || fail "smoke layout index lists no linux/$(go env GOARCH) manifest"

echo "==== lifecycle 1: image sideloaded from a local directory ===="
run_lifecycle "smoke@$digest" "$identity" "$digest" pull --from-dir "$LAYOUT" "smoke@$digest"

echo "==== lifecycle 2: image pulled from a loopback HTTP registry ===="
crane push "$LAYOUT" "127.0.0.1:$port/smoke"
run_lifecycle "127.0.0.1:$port/smoke@$digest" "$identity" "$digest" pull "127.0.0.1:$port/smoke@$digest"

echo "==== lifecycle 3: image pulled and addressed by tag ===="
run_lifecycle "127.0.0.1:$port/smoke:latest" "$identity" "$digest" pull "127.0.0.1:$port/smoke:latest"

echo "==== differential: mounted rootfs vs crane's image-level flatten ===="
# crane export flattens the image the same way knarr's merge must (docker
# export would not do: it dumps a *container*, whose runtime artifacts --
# .dockerenv, /etc/mtab, /dev stubs -- are not image content). The export is
# pinned to the host platform: knarr mounts the host-platform image, while
# crane resolves a multi-arch index to linux/amd64 by default on any host.
# Symlinks are compared as symlinks: real images carry links that only resolve
# in a running container (absolute targets, dpkg-trimmed doc trees), so
# dereferencing would produce false errors. diff checks names, content, and
# link targets; owners, modes, xattrs, and hardlink identity are covered by
# the Go e2e tests.
ref="127.0.0.1:$port/smoke@$digest"
"${KN[@]}" pull "$ref"
"${KN[@]}" mount "$ref" "$MNT"
EXPORTDIR="$(mktemp -d)"
crane --platform "linux/$(go env GOARCH)" export "$ref" - | tar -C "$EXPORTDIR" -x
diff -r -q --no-dereference "$EXPORTDIR" "$MNT" || fail "mounted rootfs differs from the flattened image"
"${KN[@]}" unmount "$MNT"
"${KN[@]}" remove "$ref"

echo "==== seal: publisher-side sealing, then the sealed image's lifecycle ===="
# Seal handles one platform per call; the smoke image is a multi-arch index,
# so select the host platform. --platform matches the index descriptors
# exactly, including the variant (busybox lists arm64 only as linux/arm64/v8),
# so read the full platform string from the index rather than assuming a bare
# os/arch pair. The printed sealed digest is the updated index's.
platform="$(crane manifest "$ref" | jq -r --arg arch "$(go env GOARCH)" \
	'first(.manifests[].platform | select(.os == "linux" and .architecture == $arch))
		| [.os, .architecture, .variant // empty] | join("/")')"
[[ "$platform" == linux/* ]] || fail "smoke index lists no linux/$(go env GOARCH) platform"
echo "== seal --dry-run (expect a sealed digest, nothing pushed) =="
dryrun="$("${KN[@]}" seal --dry-run --platform "$platform" --format json "$ref")"
sealed_dry="$(jq -r .sealed <<<"$dryrun")"
[[ "$sealed_dry" == sha256:* ]] || fail "seal --dry-run reported no sealed digest"
if crane manifest "127.0.0.1:$port/smoke@$sealed_dry" >/dev/null 2>&1; then
	fail "seal --dry-run pushed the sealed manifest"
fi

echo "== seal (expect the dry-run digest, pushed) =="
sealout="$("${KN[@]}" seal --platform "$platform" --format json "$ref")"
sealed="$(jq -r .sealed <<<"$sealout")"
[ "$sealed" = "$sealed_dry" ] || fail "sealed digest differs between dry-run and push"
[ "$sealed" != "$digest" ] || fail "sealing left the manifest digest unchanged"

echo "== raw sealed manifest (expect both fsverity annotations, bare hex) =="
# The annotations live on each platform image manifest. A multi-arch image
# seals as an index, so descend to the host platform's child first.
manifest="$(crane manifest "127.0.0.1:$port/smoke@$sealed")"
child="$sealed"
if jq -e '.manifests' <<<"$manifest" >/dev/null; then
	child="$(host_child "$manifest")"
	[[ "$child" == sha256:* ]] || fail "sealed index has no host-platform manifest"
	manifest="$(crane manifest "127.0.0.1:$port/smoke@$child")"
fi
jq -e '.annotations["composefs.merged.erofs.v1.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$manifest" >/dev/null || fail "sealed manifest lacks a well-formed merged fsverity annotation"
jq -e '.config.annotations["composefs.config.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$manifest" >/dev/null || fail "sealed manifest lacks a well-formed config fsverity annotation"

echo "==== lifecycle 4: the sealed image (pull re-verifies the seal) ===="
# $child is the sealed image's identity: the host platform's manifest when the
# seal produced an index, and the sealed manifest itself otherwise.
run_lifecycle "127.0.0.1:$port/smoke@$sealed" "$child" "$sealed" pull "127.0.0.1:$port/smoke@$sealed"

echo "SMOKE OK: purpose-built image sealed, cross-checked three ways, and run through the lifecycle; pull -> ls -> inspect -> mount -> unmount -> metadata-only overlay -> hold -> declined remove -> release -> remove, from --from-dir and from a registry, digest-pinned and by tag, plus rootfs diff against crane export and a seal round against the registry (checks passed)"
