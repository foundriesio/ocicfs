#!/bin/sh
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Assemble release artifacts into dist/ — runnable locally and from the
# release workflow, so artifact generation never becomes CI-only knowledge.
#
# Usage:
#   release-artifacts.sh version           print the artifact version string
#   release-artifacts.sh archive <arch>    pack bin/ocicfs into the .tar.gz
#   release-artifacts.sh checksums         write the checksums file over dist/
#
# The artifact version is the release tag without the leading v (0.1.0,
# 0.1.0-rc.1); an untagged checkout gets a never-releasable snapshot
# version. The .deb (built by make deb) derives its own Debian-legal
# variant of the same values.
set -eu

version() {
    tag=$(git describe --tags --exact-match --match 'v*' 2>/dev/null) \
        && printf '%s\n' "${tag#v}" \
        || printf '0.0.0-snap.g%s\n' "$(git rev-parse --short HEAD)"
}

archive() {
    arch=${1:?usage: release-artifacts.sh archive <arch>}
    ver=$(version)
    base="ocicfs_${ver}_linux_${arch}"

    stage=$(mktemp -d)
    trap 'rm -rf "$stage"' EXIT
    mkdir -p "$stage/$base" dist
    install -m 0755 bin/ocicfs "$stage/$base/ocicfs"
    install -m 0644 LICENSE README.md INSTALL.md "$stage/$base/"

    # Deterministic contents: fixed order, numeric root ownership, commit
    # time as mtime, no gzip timestamp — a retried build of the same commit
    # produces the same bytes.
    tar -C "$stage" --sort=name --owner=0 --group=0 --numeric-owner \
        --mtime="@$(git log -1 --format=%ct)" \
        -cf - "$base" | gzip -n > "dist/$base.tar.gz"
    echo "dist/$base.tar.gz"
}

checksums() {
    ver=$(version)
    out="ocicfs_${ver}_checksums.txt"
    cd dist
    rm -f "$out"
    # GitHub replaces "~" in release asset names, so an rc .deb
    # (0.1.0~rc.1) would be served under a name the checksums file does
    # not list. Apply the same substitution before checksumming; dpkg
    # reads the version from the package metadata, not the filename.
    for f in ocicfs_*~*; do
        if [ -e "$f" ]; then
            mv "$f" "$(printf '%s' "$f" | tr '~' '.')"
        fi
    done
    sha256sum ocicfs_* > "$out"
    echo "dist/$out"
}

case "${1:?usage: release-artifacts.sh version|archive|checksums}" in
version) version ;;
archive) shift; archive "$@" ;;
checksums) checksums ;;
*) echo "release-artifacts.sh: unknown mode: $1" >&2; exit 2 ;;
esac
