#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# seal-crosscheck.sh <image-uri> -- verify the merged composefs fs-verity
# digest `ocicfs seal` computes for an image against three independent
# implementations:
#
#   1. ocicfs seal --dry-run          (ocicfs's Go entry builder + EROFS writer)
#   2. cfsctl oci compute-id         (composefs-rs, the reference oracle)
#   3. cfsctl mkcomposefs --from-file (the Rust mkcomposefs fed the sealed
#      image's own dump: the drop-in for the C binary ocicfs shells out to)
#   4. umoci unpack + canonical transforms + mkcomposefs + fsverity
#      (an independent OCI flattener feeding the format-defining C writer)
#
# Legs 1, 2 and 3 must agree bit-for-bit; a mismatch fails the script. Leg 4
# matches bit-for-bit only for images without hardlinks: mkcomposefs cannot
# encode hardlinks when building from a directory, so for hardlink-carrying
# images the leg falls back to comparing `composefs-info dump` of both EROFS
# images with the hardlink encoding normalized away -- which still checks
# every path, size, mode, owner, mtime, inline payload, and content digest.
# The transforms replicated for leg 4 are the canonical ones: root directory
# metadata taken from /usr and /run emptied. The xattr allowlist (everything
# but security.capability dropped) is not replicated; an image carrying other
# xattrs shows up in the leg-4 diff.
#
# The image is pulled for the current platform; a multi-arch index resolves
# to its linux/$(go env GOARCH) child. Needs the privileged dev container
# (fs-verity for the cfsctl repository, a zot registry for ocicfs's pull,
# crane/cfsctl/umoci/mkcomposefs/composefs-info/fsverity on PATH).
#
# Intermediates are dropped as soon as their last consumer finishes — the
# oracle repository right after compute-id (only its small merged-EROFS
# object survives, for the fallback comparison), the layout after the umoci
# unpack, the unpacked bundle after mkcomposefs — and ocicfs seal itself
# streams from the registry without touching a store, so the peak footprint
# on the fs-verity store is roughly the layout plus one decompressed tree.
# A large image can still outgrow the 256M default store: run the container
# with e.g. OCICFS_TEST_VERITY_SIZE_MB=2048.
#
# Env:
#   CROSSCHECK_REGISTRY  loopback registry ocicfs seal reads from (default
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
[ -x bin/ocicfs ] || make build

# The unpacked tree and the cfsctl repository need a real filesystem --
# fs-verity for cfsctl, faithful hardlinks/mtimes for the unpack.
WORK="$(mktemp -d /var/lib/ocicfs/crosscheck-XXXXXX)"

echo "== pull $IMAGE ($PLATFORM) =="
# --platform resolves an index to the current platform but errors on a plain
# image manifest, which needs no resolution; retry without it.
crane pull --platform="$PLATFORM" --format=oci "$IMAGE" "$WORK/layout" 2>/dev/null \
	|| crane pull --format=oci "$IMAGE" "$WORK/layout"
digest="$(jq -r '.manifests[0].digest' "$WORK/layout/index.json")"
echo "manifest: $digest"

echo "== leg 1: ocicfs seal --dry-run =="
crane push "$WORK/layout" "$REGISTRY/seal-crosscheck" >/dev/null 2>&1
ocicfs_fsv="$(bin/ocicfs seal --dry-run --format json "$REGISTRY/seal-crosscheck@$digest" \
	| jq -r '.platforms[0].fsverity')"
ocicfs_hex="${ocicfs_fsv#sha256:}"
echo "ocicfs:  $ocicfs_hex"

echo "== leg 2: cfsctl oci compute-id =="
cfsctl init --algorithm fsverity-sha256-12 "$WORK/cfs-repo" >/dev/null
cfsctl --repo "$WORK/cfs-repo" oci pull "oci:$WORK/layout" crosscheck >/dev/null
cfsctl_hex="$(cfsctl --repo "$WORK/cfs-repo" oci compute-id crosscheck | tr -d '[:space:]')"
echo "cfsctl: $cfsctl_hex"
[ "$ocicfs_hex" = "$cfsctl_hex" ] || fail "ocicfs and cfsctl disagree: $ocicfs_hex vs $cfsctl_hex"
# The small merged-EROFS object is all the fallback comparison needs; the
# bulk of the oracle repository is the decompressed image content — drop it.
sealed_erofs="$WORK/cfs-repo/objects/${cfsctl_hex:0:2}/${cfsctl_hex:2}"
[ -f "$sealed_erofs" ] || fail "cfsctl repo does not hold the merged EROFS object"
cp "$sealed_erofs" "$WORK/sealed.erofs"
rm -rf "$WORK/cfs-repo"

echo "== leg 3: cfsctl mkcomposefs --from-file =="
# ocicfs builds its EROFS by piping a composefs dump through the C mkcomposefs.
# composefs-rs ships its own mkcomposefs (cfsctl's multi-call entry) meant as
# a drop-in for that binary, so the dump path must yield the same bytes under
# either implementation. The sealed image's own dump is a faithful input: the
# C tool's test suite asserts `composefs-info dump | mkcomposefs --from-file`
# reproduces an image bit-for-bit.
composefs-info dump "$WORK/sealed.erofs" >"$WORK/sealed.dump"
cfsctl mkcomposefs --min-version=1 --max-version=1 --from-file "$WORK/sealed.dump" "$WORK/rust.erofs"
rust_hex="$(fsverity digest "$WORK/rust.erofs" | awk '{print $1}')"
rust_hex="${rust_hex#sha256:}"
echo "cfsctl mkcomposefs: $rust_hex"
cmp -s "$WORK/sealed.erofs" "$WORK/rust.erofs" \
	|| fail "the Rust mkcomposefs rebuilt the sealed dump into different bytes: $ocicfs_hex vs $rust_hex"
rm -f "$WORK/rust.erofs"

echo "== leg 4: umoci unpack + canonical transforms + mkcomposefs =="
# umoci addresses layouts by tag; crane layouts carry no ref.name annotation.
jq '.manifests[0].annotations = {"org.opencontainers.image.ref.name": "crosscheck"}' \
	"$WORK/layout/index.json" >"$WORK/index.json" && mv "$WORK/index.json" "$WORK/layout/index.json"
umoci unpack --image "$WORK/layout:crosscheck" "$WORK/bundle" >/dev/null
# The unpack was the layout's last consumer.
rm -rf "$WORK/layout"
rootfs="$WORK/bundle/rootfs"
# The canonical transforms: root metadata comes from /usr, and an existing
# /run is emptied and shares that metadata (a missing /run stays missing).
[ -d "$rootfs/usr" ] || fail "image has no /usr; ocicfs builds it non-canonically and seal declines it"
chown --reference="$rootfs/usr" "$rootfs"
chmod --reference="$rootfs/usr" "$rootfs"
if [ -d "$rootfs/run" ]; then
	find "$rootfs/run" -mindepth 1 -delete
	touch -r "$rootfs/usr" "$rootfs/run"
fi
touch -r "$rootfs/usr" "$rootfs"
# Pin the stable V1 EROFS format, the one ocicfs and the cfsctl oracle write;
# the mkcomposefs default is a different format version with different bytes.
mkcomposefs --min-version=1 --max-version=1 "$rootfs" "$WORK/umoci.erofs"
# The EROFS is written; only the two metadata images are needed from here on.
rm -rf "$WORK/bundle"
umoci_hex="$(fsverity digest "$WORK/umoci.erofs" | awk '{print $1}')"
umoci_hex="${umoci_hex#sha256:}"
echo "mkcomposefs: $umoci_hex"

if [ "$umoci_hex" = "$ocicfs_hex" ]; then
	echo "RESULT: all four digests MATCH"
	exit 0
fi

# Expected for images with hardlinks: mkcomposefs cannot encode them from a
# directory. Prove the trees are identical apart from that encoding.
normalize() {
	awk '{ sub(/^@/, "", $3); $4 = 1; if ($NF != "-") $9 = substr($NF, 1, 2) "/" substr($NF, 3); print }' "$1"
}
composefs-info dump "$WORK/umoci.erofs" >"$WORK/umoci.dump"
normalize "$WORK/sealed.dump" >"$WORK/sealed.norm"
normalize "$WORK/umoci.dump" >"$WORK/umoci.norm"
if diff -q "$WORK/sealed.norm" "$WORK/umoci.norm" >/dev/null; then
	echo "RESULT: legs 1+2+3 MATCH; leg 4 differs only in hardlink encoding" \
		"(trees identical across $(wc -l <"$WORK/sealed.norm") entries)"
	exit 0
fi
echo "normalized dump diff (first 20 lines):" >&2
diff "$WORK/sealed.norm" "$WORK/umoci.norm" | head -20 >&2
fail "the independently built tree differs from the sealed image"
