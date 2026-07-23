#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# seal-crosscheck.sh <image-uri> -- verify the merged composefs fs-verity
# digest `knarr seal` computes for an image against two independent
# implementations:
#
#   1. knarr seal --dry-run          (knarr's Go entry builder + EROFS writer)
#   2. cfsctl oci compute-id         (composefs-rs, the reference oracle)
#   3. umoci unpack + canonical transforms + mkcomposefs + fsverity
#      (an independent OCI flattener feeding the format-defining C writer)
#
# Legs 1 and 2 must agree bit-for-bit; a mismatch fails the script. Leg 3
# matches bit-for-bit only for images without hardlinks: mkcomposefs cannot
# encode hardlinks when building from a directory, so for hardlink-carrying
# images the leg falls back to comparing `composefs-info dump` of both EROFS
# images with the hardlink encoding normalized away -- which still checks
# every path, size, mode, owner, mtime, inline payload, and content digest.
# The transforms replicated for leg 3 are the canonical ones: root directory
# metadata taken from /usr and /run emptied. The xattr allowlist (everything
# but security.capability dropped) is not replicated; an image carrying other
# xattrs shows up in the leg-3 diff.
#
# The image is pulled for the current platform; a multi-arch index resolves
# to its linux/$(go env GOARCH) child. Needs the privileged dev container
# (fs-verity for the cfsctl repository, a zot registry for knarr's pull,
# crane/cfsctl/umoci/mkcomposefs/composefs-info/fsverity on PATH).
#
# Intermediates are dropped as soon as their last consumer finishes — the
# oracle repository right after compute-id (only its small merged-EROFS
# object survives, for the fallback comparison), the layout after the umoci
# unpack, the unpacked bundle after mkcomposefs — and knarr seal itself
# streams from the registry without touching a store, so the peak footprint
# on the fs-verity store is roughly the layout plus one decompressed tree.
# A large image can still outgrow the 256M default store: run the container
# with e.g. KNARR_TEST_VERITY_SIZE_MB=2048.
#
# Env:
#   CROSSCHECK_REGISTRY  loopback registry knarr seal reads from (default
#                        127.0.0.1:5000, the dev container's persistent zot;
#                        the smoke test points it at its own instance)
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <image-uri>" >&2; exit 2; }
IMAGE="$1"
PLATFORM="linux/$(go env GOARCH)"
REGISTRY="${CROSSCHECK_REGISTRY:-127.0.0.1:5000}"

WORK=""
cleanup() {
	set +e
	[ -n "$WORK" ] && rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "CROSSCHECK FAIL: $*" >&2; exit 1; }

for tool in crane cfsctl umoci mkcomposefs composefs-info fsverity jq; do
	command -v "$tool" >/dev/null || fail "$tool not on PATH (run inside the dev container)"
done
curl -fsS "http://$REGISTRY/v2/" >/dev/null 2>&1 || fail "no registry at $REGISTRY (the dev container serves one)"
[ -x bin/knarr ] || make build

# The unpacked tree and the cfsctl repository need a real filesystem --
# fs-verity for cfsctl, faithful hardlinks/mtimes for the unpack.
WORK="$(mktemp -d /var/lib/knarr/crosscheck-XXXXXX)"

echo "== pull $IMAGE ($PLATFORM) =="
# --platform resolves an index to the current platform but errors on a plain
# image manifest, which needs no resolution; retry without it.
crane pull --platform="$PLATFORM" --format=oci "$IMAGE" "$WORK/layout" 2>/dev/null \
	|| crane pull --format=oci "$IMAGE" "$WORK/layout"
digest="$(jq -r '.manifests[0].digest' "$WORK/layout/index.json")"
echo "manifest: $digest"

echo "== leg 1: knarr seal --dry-run =="
crane push "$WORK/layout" "$REGISTRY/seal-crosscheck" >/dev/null 2>&1
knarr_fsv="$(bin/knarr seal --dry-run --format json "$REGISTRY/seal-crosscheck@$digest" \
	| jq -r '.platforms[0].fsverity')"
knarr_hex="${knarr_fsv#sha256:}"
echo "knarr:  $knarr_hex"

echo "== leg 2: cfsctl oci compute-id =="
cfsctl init --algorithm fsverity-sha256-12 "$WORK/cfs-repo" >/dev/null
cfsctl --repo "$WORK/cfs-repo" oci pull "oci:$WORK/layout" crosscheck >/dev/null
cfsctl_hex="$(cfsctl --repo "$WORK/cfs-repo" oci compute-id crosscheck | tr -d '[:space:]')"
echo "cfsctl: $cfsctl_hex"
[ "$knarr_hex" = "$cfsctl_hex" ] || fail "knarr and cfsctl disagree: $knarr_hex vs $cfsctl_hex"
# The small merged-EROFS object is all the fallback comparison needs; the
# bulk of the oracle repository is the decompressed image content — drop it.
sealed_erofs="$WORK/cfs-repo/objects/${cfsctl_hex:0:2}/${cfsctl_hex:2}"
[ -f "$sealed_erofs" ] || fail "cfsctl repo does not hold the merged EROFS object"
cp "$sealed_erofs" "$WORK/sealed.erofs"
rm -rf "$WORK/cfs-repo"

echo "== leg 3: umoci unpack + canonical transforms + mkcomposefs =="
# umoci addresses layouts by tag; crane layouts carry no ref.name annotation.
jq '.manifests[0].annotations = {"org.opencontainers.image.ref.name": "crosscheck"}' \
	"$WORK/layout/index.json" >"$WORK/index.json" && mv "$WORK/index.json" "$WORK/layout/index.json"
umoci unpack --image "$WORK/layout:crosscheck" "$WORK/bundle" >/dev/null
# The unpack was the layout's last consumer.
rm -rf "$WORK/layout"
rootfs="$WORK/bundle/rootfs"
# The canonical transforms: root metadata comes from /usr, and an existing
# /run is emptied and shares that metadata (a missing /run stays missing).
[ -d "$rootfs/usr" ] || fail "image has no /usr; knarr builds it non-canonically and seal declines it"
chown --reference="$rootfs/usr" "$rootfs"
chmod --reference="$rootfs/usr" "$rootfs"
if [ -d "$rootfs/run" ]; then
	find "$rootfs/run" -mindepth 1 -delete
	touch -r "$rootfs/usr" "$rootfs/run"
fi
touch -r "$rootfs/usr" "$rootfs"
# Pin the stable V1 EROFS format, the one knarr and the cfsctl oracle write;
# the mkcomposefs default is a different format version with different bytes.
mkcomposefs --min-version=1 --max-version=1 "$rootfs" "$WORK/third.erofs"
# The EROFS is written; only the two metadata images are needed from here on.
rm -rf "$WORK/bundle"
third_hex="$(fsverity digest "$WORK/third.erofs" | awk '{print $1}')"
third_hex="${third_hex#sha256:}"
echo "mkcomposefs: $third_hex"

if [ "$third_hex" = "$knarr_hex" ]; then
	echo "RESULT: all three digests MATCH"
	exit 0
fi

# Expected for images with hardlinks: mkcomposefs cannot encode them from a
# directory. Prove the trees are identical apart from that encoding.
normalize() {
	awk '{ sub(/^@/, "", $3); $4 = 1; if ($NF != "-") $9 = substr($NF, 1, 2) "/" substr($NF, 3); print }' "$1"
}
composefs-info dump "$WORK/sealed.erofs" >"$WORK/sealed.dump"
composefs-info dump "$WORK/third.erofs" >"$WORK/third.dump"
normalize "$WORK/sealed.dump" >"$WORK/sealed.norm"
normalize "$WORK/third.dump" >"$WORK/third.norm"
if diff -q "$WORK/sealed.norm" "$WORK/third.norm" >/dev/null; then
	echo "RESULT: legs 1+2 MATCH; leg 3 differs only in hardlink encoding" \
		"(trees identical across $(wc -l <"$WORK/sealed.norm") entries)"
	exit 0
fi
echo "normalized dump diff (first 20 lines):" >&2
diff "$WORK/sealed.norm" "$WORK/third.norm" | head -20 >&2
fail "the independently built tree differs from the sealed image"
