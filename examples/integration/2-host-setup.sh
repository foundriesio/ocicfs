#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Host/device setup: docs/integration.md#hostdevice-setup
#
# Installs cfs-snapshotter, connects it to containerd and Docker, and
# enables preloaded mode and fs-verity.
#
# Needs: CFS_SNAPSHOTTER_VERSION

set -eux -o pipefail

mkdir -p work
arch=$(dpkg --print-architecture)

# Enable fs-verity: put the store on an ext4 filesystem that supports it.
# Do this before installing the snapshotter. It looks at the store when it
# starts, and runs without fs-verity if the filesystem cannot provide it.
if ! mountpoint /var/lib/ocicfs; then
	sudo fallocate --length 2G /var/lib/ocicfs.ext4
	sudo mkfs.ext4 -F -b 4096 -O verity /var/lib/ocicfs.ext4
	sudo mkdir -p /var/lib/ocicfs
	sudo mount -o loop /var/lib/ocicfs.ext4 /var/lib/ocicfs
fi

# Install cfs-snapshotter from its release. The checksums file lists every
# release file; --ignore-missing checks only the one downloaded.
deb="cfs-snapshotter_${CFS_SNAPSHOTTER_VERSION}_${arch}.deb"
checksums="cfs-snapshotter_${CFS_SNAPSHOTTER_VERSION}_checksums.txt"
release="https://github.com/foundriesio/cfs-snapshotter/releases/download/v$CFS_SNAPSHOTTER_VERSION"
curl -fsSL --output-dir work -O "$release/$deb" -O "$release/$checksums"
(cd work && sha256sum --check --ignore-missing "$checksums")
sudo apt-get install -y "./work/$deb"

# Enable preloaded mode: the snapshotter serves only images that are
# already in the ocicfs store, and containerd keeps no copy of their
# layers. Delta pulls need this mode.
echo 'CFS_OPTS="-admit=preloaded"' | sudo tee /etc/default/cfs-snapshotter

# Connect containerd: copy the supplied configuration to a drop-in
# directory and import it. `imports` must come before any [section].
sudo mkdir -p /etc/containerd/conf.d
sudo cp /usr/share/doc/cfs-snapshotter/containerd-cfs.toml /etc/containerd/conf.d/
if ! grep '^imports' /etc/containerd/config.toml; then
	sudo sed -i '1i imports = ["/etc/containerd/conf.d/*.toml"]' /etc/containerd/config.toml
fi
sudo systemctl restart containerd
sudo systemctl restart cfs-snapshotter

# Connect Docker: use containerd's image store with the cfs snapshotter.
# With lazy-pull, Docker 29.9 or newer skips the layers of a preloaded
# image. The settings are merged into the existing daemon.json.
if [ ! -f /etc/docker/daemon.json ]; then
	echo '{}' | sudo tee /etc/docker/daemon.json
fi
jq '.features["containerd-snapshotter"] = true
	| .features["lazy-pull"] = true
	| .["storage-driver"] = "cfs"' /etc/docker/daemon.json >work/daemon.json
sudo cp work/daemon.json /etc/docker/daemon.json
sudo systemctl restart docker

# Check the setup: the snapshotter is wired up, the store runs with
# fs-verity, and Docker uses the snapshotter.
sudo containerd-cfs-grpc -check -admit=preloaded | grep 'check passed'
sudo ocicfs version | grep 'verity: enabled'
docker info --format '{{.Driver}}' | grep cfs
