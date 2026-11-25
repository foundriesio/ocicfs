#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prerequisites. Not a stage of the guide.
#
# Installs what a stock Ubuntu 24.04 machine lacks for the stages that
# follow. This one machine acts as both the publishing machine and the
# device, so ocicfs is installed once here for both.
#
# Needs: DOCKER_PACKAGE, CONTAINERD_PACKAGE   apt package versions
#        COMPOSEFS_VERSION, SKOPEO_VERSION, OCICFS_VERSION

set -eux -o pipefail

mkdir -p work
arch=$(dpkg --print-architecture)

# Docker 29 and containerd 2.1 or newer, from Docker's apt repository.
if ! grep -rqs download.docker.com /etc/apt/sources.list.d; then
	. /etc/os-release
	sudo mkdir -p /etc/apt/keyrings
	sudo curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
	echo "deb [arch=$arch signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$ID $VERSION_CODENAME stable" |
		sudo tee /etc/apt/sources.list.d/docker.list
fi
sudo apt-get update
# --allow-downgrades: the machine may already have a newer version than
# the pinned one.
sudo apt-get install -y --allow-downgrades \
	"docker-ce=$DOCKER_PACKAGE" "docker-ce-cli=$DOCKER_PACKAGE" \
	"containerd.io=$CONTAINERD_PACKAGE" docker-buildx-plugin jq

# mkcomposefs, which `ocicfs pull` and `ocicfs seal` run. Ubuntu 24.04 has
# no package for it, so build it.
if ! command -v mkcomposefs; then
	sudo apt-get install -y gcc git libssl-dev meson ninja-build pkg-config
	git clone --depth 1 --branch "v$COMPOSEFS_VERSION" \
		https://github.com/composefs/composefs.git work/composefs
	meson setup work/composefs/build work/composefs -Dfuse=disabled -Dman=disabled
	meson compile -C work/composefs/build
	sudo meson install -C work/composefs/build
	sudo ldconfig
fi

# A skopeo that has --dest-force-compress-format. Ubuntu 24.04's is too
# old, so this wrapper runs a newer one from its container image, with the
# registry login of the user who calls it. Only SKOPEO_VERSION is filled
# in here: the escaped \$HOME and \$@ are expanded when the wrapper runs.
if ! skopeo copy --help | grep -e --dest-force-compress-format; then
	sudo tee /usr/local/bin/skopeo <<EOF_WRAPPER
#!/bin/sh
exec docker run --rm \\
	--volume "\$HOME/.docker/config.json:/auth.json:ro" \\
	--env REGISTRY_AUTH_FILE=/auth.json \\
	quay.io/skopeo/stable:v$SKOPEO_VERSION "\$@"
EOF_WRAPPER
	sudo chmod +x /usr/local/bin/skopeo
fi

# ocicfs, from its release. The checksums file lists every release file;
# --ignore-missing checks only the one downloaded.
deb="ocicfs_${OCICFS_VERSION}_${arch}.deb"
checksums="ocicfs_${OCICFS_VERSION}_checksums.txt"
release="https://github.com/foundriesio/ocicfs/releases/download/v$OCICFS_VERSION"
curl -fsSL --output-dir work -O "$release/$deb" -O "$release/$checksums"
(cd work && sha256sum --check --ignore-missing "$checksums")
sudo apt-get install -y "./work/$deb"
