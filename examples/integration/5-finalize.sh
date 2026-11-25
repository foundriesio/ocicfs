#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Updating, preloaded mode, step 5, finalizing: docs/integration.md#updating
#
# Run this after 4-update.sh once the new version is good: it removes
# version 1, which was kept stopped until now for a rollback. The
# alternative, going back to version 1, is 5-rollback.sh.
#
# Needs: work/v1.ref, work/v2.ref    from 1-publish.sh

set -eux -o pipefail

v1=$(cat work/v1.ref)
v2=$(cat work/v2.ref)

# retry runs a command until it succeeds: up to 30 times, two seconds apart.
retry() {
	for _ in $(seq 30); do
		if "$@"; then
			return
		fi
		sleep 2
	done
	return 1
}

# Remove version 1: its stopped container, its Docker image, its reference
# in the store, then its files. The snapshotter lets go of the image a
# moment after `docker image rm` returns. Until then `ocicfs remove`
# fails, and `ocicfs gc` can report the store busy, so both are retried.
# Files that version 2 shares with version 1 stay.
docker rm app-v1
docker image rm "$v1"
retry sudo ocicfs remove "$v1"
retry sudo ocicfs gc

# Show what is left in the store, and check that version 1 is gone and
# that the workload still runs version 2.
sudo ocicfs ls
if sudo ocicfs inspect "$v1"; then
	echo "Version 1 is still in the store." >&2
	exit 1
fi
"$(dirname "$0")/check.sh" "$v2" v2
