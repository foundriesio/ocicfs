#!/usr/bin/env bash
# Container entrypoint for the knarr dev / CI image.
#
# fs-verity needs a real backing filesystem (tmpfs and overlayfs cannot provide
# it), so when running privileged this creates a loop-backed ext4 filesystem with
# the verity feature and mounts it at the knarr default data root. Manual
# `knarr pull` and the privileged end-to-end tests use it directly; when the mount
# cannot be created (unprivileged run, missing tooling) the tests detect the
# absence and skip.
#
# It is idempotent and execs the container's command afterwards.
#
# Env:
#   KNARR_TEST_VERITY_SIZE_MB   loop image size in MiB (default 256)

set -euo pipefail

# The knarr default data root, shared with the e2e tests (internal/devtest.verityDir).
ROOT="/var/lib/knarr"
IMG="/var/tmp/knarr-verity.img"
SIZE_MB="${KNARR_TEST_VERITY_SIZE_MB:-256}"

setup_verity_mount() {
	# Already mounted (container restart or repeated invocation).
	if mountpoint -q "$ROOT" 2>/dev/null; then
		return 0
	fi
	if ! command -v losetup >/dev/null || ! command -v mkfs.ext4 >/dev/null; then
		return 1
	fi
	# Privileged check: can we allocate a loop device?
	if ! losetup --find >/dev/null 2>&1; then
		return 1
	fi

	# Detach stale loops backed by our image path from previous runs.
	losetup -a 2>/dev/null | grep -F "$IMG" | cut -d: -f1 | while read -r d; do
		losetup -d "$d" 2>/dev/null || true
	done

	mkdir -p "$ROOT"
	dd if=/dev/zero of="$IMG" bs=1M count="$SIZE_MB" status=none
	# ext4 needs a 4096-byte block size for the verity feature.
	mkfs.ext4 -q -F -b 4096 -O verity "$IMG"
	local dev
	dev="$(losetup --find --show "$IMG")" || return 1
	mount "$dev" "$ROOT" || {
		losetup -d "$dev" 2>/dev/null || true
		return 1
	}
}

if setup_verity_mount; then
	echo "dev-entrypoint: ext4 (verity) mounted at $ROOT (${SIZE_MB}M)" >&2
else
	echo "dev-entrypoint: no fs-verity mount at $ROOT (unprivileged or tooling missing); e2e tests will skip" >&2
fi

# With no command (e.g. a lifecycle hook), exit success after setup.
if [ "$#" -eq 0 ]; then
	exit 0
fi
exec "$@"
