#!/bin/sh
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Install-test the built .deb on clean Debian and Ubuntu containers
# (Makefile deb-test target). For each distro: assert the control fields,
# install, check the stamped version, reinstall (the upgrade path), then
# remove and purge. Expects DEB_VERSION and VERSION in the environment and
# the .deb directory as the only argument.
set -eu

out_dir=${1:?usage: deb-test.sh <deb-dir>}
: "${DEB_VERSION:?}" "${VERSION:?}"

arch=$(dpkg --print-architecture 2>/dev/null \
    || uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
deb="$out_dir/knarr_${DEB_VERSION}_${arch}.deb"
[ -f "$deb" ] || { echo "deb-test: missing $deb" >&2; exit 1; }

for image in debian:trixie ubuntu:noble; do
    echo "deb-test: $image"
    docker run --rm \
        -v "$(realpath "$out_dir")":/out:ro \
        -e DEB_VERSION -e VERSION -e ARCH="$arch" \
        "$image" bash -s <<'EOF'
set -eux
deb="/out/knarr_${DEB_VERSION}_${ARCH}.deb"

[ "$(dpkg-deb -f "$deb" Package)" = knarr ]
[ "$(dpkg-deb -f "$deb" Version)" = "$DEB_VERSION" ]
[ "$(dpkg-deb -f "$deb" Architecture)" = "$ARCH" ]
dpkg-deb -f "$deb" Recommends | grep -q composefs

apt-get update
apt-get install -y --no-install-recommends "$deb"
knarr --version | grep -F "knarr ${VERSION}"
knarr --help >/dev/null

# Installing the same package again exercises the upgrade path.
apt-get install -y --reinstall --no-install-recommends "$deb"
knarr --version >/dev/null

apt-get remove -y knarr
! command -v knarr
apt-get purge -y knarr
EOF
done
echo "deb-test: OK"
