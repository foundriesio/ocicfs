# Installing knarr

knarr runs on Linux, amd64 and arm64. No Go toolchain is needed to install
a release. Pick the path that matches your system:

| Your system                        | Install path                        |
| ---------------------------------- | ----------------------------------- |
| Debian or Ubuntu based             | [1. Debian package](#1-debian-package) |
| any other Linux                    | [2. Binary archive](#2-binary-archive) |
| building yourself (Go 1.26+)       | [3. From source](#3-from-source)    |

Release artifacts live on the
[releases page](https://github.com/foundriesio/knarr/releases). `<ver>` is
the release version (e.g. `0.1.0`), `<arch>` is `amd64` or `arm64`.

## 1. Debian package

Download `knarr_<ver>_<arch>.deb` and `knarr_<ver>_checksums.txt`, then:

```sh
sha256sum --check --ignore-missing knarr_<ver>_checksums.txt
sudo apt install ./knarr_<ver>_<arch>.deb
```

This installs `/usr/bin/knarr` plus bash and zsh completions.

- **Upgrade:** `sudo apt install ./knarr_<newver>_<arch>.deb`
- **Rollback:** `sudo apt install --allow-downgrades ./knarr_<oldver>_<arch>.deb`
- **Uninstall:** `sudo apt remove knarr`

## 2. Binary archive

Download `knarr_<ver>_linux_<arch>.tar.gz` and
`knarr_<ver>_checksums.txt`, then:

```sh
sha256sum --check --ignore-missing knarr_<ver>_checksums.txt
tar -xzf knarr_<ver>_linux_<arch>.tar.gz
sudo install -m 0755 knarr_<ver>_linux_<arch>/knarr /usr/local/bin/knarr
```

The archive also carries `LICENSE`, `README.md`, and this file.

- **Upgrade / rollback:** install the other version's binary the same way.
- **Uninstall:** `sudo rm /usr/local/bin/knarr`

## 3. From source

Needs Go 1.26 or newer. Build from a git checkout, not a source tarball, so
the binary reports its real version:

```sh
git clone https://github.com/foundriesio/knarr.git
cd knarr
make build
sudo make install
```

`make install` places the binary under `/usr/local/bin`; override with
`PREFIX=/usr` or `DESTDIR=<staging>` for packaging.

- **Uninstall:** `sudo rm /usr/local/bin/knarr` (or your `PREFIX`)

## Check the installation

```sh
knarr --version
```

prints `knarr v<ver> (<commit>)` for a release build.

## Runtime requirements

Installation works on any Linux; *using* knarr needs:

- **Kernel: Linux 6.6 or newer** for mounting — EROFS, fs-verity, and
  overlayfs with data-only lower layers and verity validation. A vendor
  kernel carrying the same support also works; [docs/mount.md](docs/mount.md)
  lists the exact mainline commits.
- **A store filesystem with fs-verity enabled.** ext4 needs a 4096-byte
  block size and the `verity` feature; btrfs supports fs-verity out of the
  box. [docs/mount.md](docs/mount.md) shows a loopback-ext4 fallback for
  hosts without one.
- **`mkcomposefs`** (from the
  [composefs](https://github.com/composefs/composefs) project) — needed
  only by `pull` and `seal`; all other commands work without it. The .deb
  declares the `composefs` package as a Recommends because it is not yet
  in the Debian-stable or Ubuntu archives; install it from your
  distribution when available, or build it from source.
- **Root privileges** for the verbs that mount, use loop devices, or
  enable fs-verity (`pull`, `mount`, `unmount`, `gc`).

## Verifying provenance (optional)

Beyond checksums, every artifact carries a GitHub build-provenance
attestation — proof it was built by this repository's release workflow from
the tagged commit (needs the [GitHub CLI](https://cli.github.com/)):

```sh
gh attestation verify knarr_<ver>_linux_<arch>.tar.gz --repo foundriesio/knarr
```

An SPDX SBOM per binary (`knarr_<ver>_linux_<arch>.spdx.json`) is attached
to each release.

## First steps

Pull an image from Docker Hub, mount it, look inside, unmount:

```sh
sudo knarr pull ubuntu          # docker.io/library/ubuntu:latest
sudo mkdir -p /mnt/image
sudo knarr mount ubuntu /mnt/image
ls /mnt/image
sudo knarr unmount /mnt/image
```

In production, pull digest-pinned references (`<image>@sha256:<digest>`) —
the digest is the trust anchor; see [docs/pull.md](docs/pull.md). The
[README](README.md) has the full documentation index — pulling, mounting,
the on-disk store layout, and garbage collection.
