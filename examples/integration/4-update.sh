#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Updating, preloaded mode, steps 1 to 4: docs/integration.md#updating
#
# Updates the workload from version 1 to version 2 with a delta pull.
# Afterwards, run 5-finalize.sh to keep the new version, or 5-rollback.sh
# to go back to the old one.
#
# Needs: work/v2.ref    from 1-publish.sh

set -eux -o pipefail

v2=$(cat work/v2.ref)

# 1. Keep the running version in the store, as the base of the delta.
# Nothing to do: version 1 was pulled by digest, and that reference keeps
# it. An image deployed by tag needs `ocicfs hold` here.

# 2. Pull the new version with a delta pull. ocicfs runs as root, so point
# it at your registry login.
sudo env DOCKER_CONFIG="$HOME/.docker" ocicfs pull --delta "$v2" 2>&1 | tee work/pull.log

# Check the delta. ocicfs logs one line for each layer, such as:
#
#   msg="delta: layer" layer=sha256:... size=354918 missing=1 requests=1 fetched=439 fallback=none
#
# Take the layer that had files missing from the store (missing is not 0).
# It must have been fetched with range requests (requests is not 0), and
# without falling back to reading the whole layer (fallback=none).
grep 'delta: layer' work/pull.log | grep --invert-match 'missing=0 ' |
	grep 'requests=[1-9].* fallback=none'

# Show the saving. The last log line sums up the pull: the bytes fetched,
# next to the total size of the image's layers.
grep 'delta: pull' work/pull.log |
	sed -E 's/.* size=([0-9]+) .* fetched=([0-9]+).*/delta pull fetched \2 of \1 bytes/'

# 3. Register the new version with Docker, as in preloading: this
# downloads its manifest and config, not its layers.
docker pull "$v2"

# 4. Switch the workload to the new version and check it. Stop the
# container of version 1 but keep it, along with its image: until the new
# version is confirmed, a rollback only has to start it again.
# As in 3-preload.sh, the new container runs without network.
docker stop app-v1
docker run --detach --name app-v2 --network none "$v2"
"$(dirname "$0")/check.sh" "$v2" v2
