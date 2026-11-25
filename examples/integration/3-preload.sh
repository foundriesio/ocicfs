#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Preloading: docs/integration.md#preloading
#
# Preloads version 1 from its archive and starts it.
#
# Needs: work/v1.tar, work/v1.ref    from 1-publish.sh

set -eux -o pipefail

v1=$(cat work/v1.ref)

# 1. Import the archive into the store. The reference names the digest
# that the archive must contain. `unshare --net` runs ocicfs with no
# network at all, to show that the image content comes from the archive.
sudo unshare --net ocicfs pull --from-tar work/v1.tar "$v1"

# Check that the store keeps each file once. The image has the same large
# file in two layers, so the store must be smaller than the image's root
# filesystem.
rootfs_bytes=$(sudo ocicfs inspect --format json "$v1" | jq .rootfs_size)
store_bytes=$(sudo du --summarize --bytes /var/lib/ocicfs/objects | cut -f1)
[ "$store_bytes" -lt "$rootfs_bytes" ]

# 2. Register the image with Docker. The image is already in the store, so
# `docker pull` downloads its manifest and config, not its layers. It
# needs the registry for that, so the first start is not fully offline.
# `docker load` of the archive would be, but it keeps a copy of the
# archive's layers in containerd's content store.
docker pull "$v1"

# 3. Start the workload and check it. `--network none` gives the container
# no network, to show that running an image needs no registry once the
# image is in the store and registered. Leave the option out for a
# workload that uses the network itself.
docker run --detach --name app-v1 --network none "$v1"
"$(dirname "$0")/check.sh" "$v1" v1
