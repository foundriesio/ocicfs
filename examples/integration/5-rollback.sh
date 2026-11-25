#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Updating, preloaded mode, step 5, rolling back: docs/integration.md#updating
#
# Run this after 4-update.sh if the new version is not good: it switches
# the workload back to version 1 and removes version 2. The alternative,
# keeping the new version, is 5-finalize.sh.
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

# Switch the workload back and check it. The container of version 1 was
# only stopped, so there is nothing to download or create: stop the new
# container and start the old one again.
docker stop app-v2
docker start app-v1
"$(dirname "$0")/check.sh" "$v1" v1

# Remove version 2: its container, its Docker image, its reference in the
# store, then its files. The snapshotter lets go of the image a moment
# after `docker image rm` returns. Until then `ocicfs remove` fails, and
# `ocicfs gc` can report the store busy, so both are retried.
docker rm app-v2
docker image rm "$v2"
retry sudo ocicfs remove "$v2"
retry sudo ocicfs gc

# Show what is left in the store, and check that version 2 is gone.
sudo ocicfs ls
if sudo ocicfs inspect "$v2"; then
	echo "Version 2 is still in the store." >&2
	exit 1
fi
