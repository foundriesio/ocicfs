#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks the running container of a version, named app-<version>.
#
# Usage: check.sh <image reference> <version>

set -eux -o pipefail

image=$1
version=$2
container="app-$version"
# The manifest digest: the part of the reference after the @.
digest=${image#*@}

# The container reports the expected version.
[ "$(docker exec "$container" cat /app/version)" = "$version" ]

# It runs the pinned digest.
[ "$(docker inspect --format '{{.Image}}' "$container")" = "$digest" ]

# Its root filesystem is mounted with verity=require, so file contents
# are verified when read. In mountinfo, " / / " selects the mount whose
# root and mount point are both /: the container's root filesystem.
pid=$(docker inspect --format '{{.State.Pid}}' "$container")
sudo grep ' / / ' "/proc/$pid/mountinfo" | grep --only-matching 'verity=require'

# The publisher's seal, an annotation of the image manifest, equals the
# fs-verity digest of the image the store built. `moby` is Docker's
# namespace in containerd, where it keeps the manifest.
seal=$(sudo ctr --namespace moby content get "$digest" |
	jq -r '.annotations["composefs.merged.erofs.v1.fsverity-sha256-12"]')
built=$(sudo ocicfs inspect --format json "$image" | jq -r .fsverity)
[ "sha256:$seal" = "$built" ]

# Docker downloaded none of the image's layers: containerd's content store
# holds the manifest but no layer that it lists. $layers has one digest on
# each line, and grep looks for any of them.
layers=$(sudo ctr --namespace moby content get "$digest" | jq -r '.layers[].digest')
if sudo ctr --namespace moby content ls --quiet | grep --fixed-strings "$layers"; then
	echo "containerd's content store holds layers of $image." >&2
	exit 1
fi

echo "ok: $container runs $image"
