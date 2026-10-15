#!/usr/bin/env bash
# Container entrypoint for the ocicfs dev / CI image.
#
# fs-verity needs a real backing filesystem (tmpfs and overlayfs cannot provide
# it), so when running privileged this creates a loop-backed ext4 filesystem with
# the verity feature and mounts it at the ocicfs default data root. Manual
# `ocicfs pull` and the privileged end-to-end tests use it directly; when the mount
# cannot be created (unprivileged run, missing tooling) the tests detect the
# absence and skip.
#
# It is idempotent and execs the container's command afterwards.
#
# Env:
#   OCICFS_TEST_VERITY_SIZE_MB   loop image size in MiB (default 256)

set -euo pipefail

# The ocicfs default data root, shared with the e2e tests (internal/devtest.verityDir).
ROOT="/var/lib/ocicfs"
IMG="/var/tmp/ocicfs-verity.img"
SIZE_MB="${OCICFS_TEST_VERITY_SIZE_MB:-256}"

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

# The persistent zot registry: a local OCI registry at 127.0.0.1:5000 for
# manual push -> seal -> pull testing (`ocicfs seal 127.0.0.1:5000/...`). Its
# storage lives outside the verity mount, so registry state is independent of
# it; the smoke test never uses this instance — it runs its own ephemeral one
# on another port. Idempotent: an instance already answering keeps running.
start_zot() {
	command -v zot >/dev/null || return 1
	[ -f /etc/zot/config.json ] || return 1
	(
		# Serialize concurrent invocations (container boot racing a lifecycle
		# hook): the probe-then-start below is not atomic, and a second zot
		# does not exit on the conflict — it blocks forever on the storage
		# lock before ever reaching the port bind.
		flock -w 30 9 || exit 1
		if curl -fsS http://127.0.0.1:5000/v2/ >/dev/null 2>&1; then
			exit 0
		fi
		mkdir -p /var/lib/zot
		# setsid detaches zot into its own session: a devcontainer lifecycle
		# hook kills its process group when this script exits, which would
		# take a plainly backgrounded daemon down with it. 9>&- keeps the
		# daemon from inheriting (and so holding) the start lock.
		setsid zot serve /etc/zot/config.json >/var/tmp/zot.log 2>&1 </dev/null 9>&- &
		# The first boot can take several seconds before the listener is up
		# (fresh storage init), so give it a generous window.
		for _ in $(seq 100); do
			curl -fsS http://127.0.0.1:5000/v2/ >/dev/null 2>&1 && exit 0
			sleep 0.1
		done
		exit 1
	) 9>/var/tmp/zot-start.lock
}

if setup_verity_mount; then
	echo "dev-entrypoint: ext4 (verity) mounted at $ROOT (${SIZE_MB}M)" >&2
else
	echo "dev-entrypoint: no fs-verity mount at $ROOT (unprivileged or tooling missing); e2e tests will skip" >&2
fi

if start_zot; then
	echo "dev-entrypoint: zot registry serving at 127.0.0.1:5000 (log: /var/tmp/zot.log)" >&2
else
	echo "dev-entrypoint: zot registry not started (binary or config missing, or startup timed out)" >&2
fi

# With no command (e.g. a lifecycle hook), exit success after setup.
if [ "$#" -eq 0 ]; then
	exit 0
fi
exec "$@"
