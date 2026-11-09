# Installing ocicfs

ocicfs runs on Linux, amd64 and arm64. No Go toolchain is needed to install
a release. Pick the path that matches your system:

| Your system                        | Install path                        |
| ---------------------------------- | ----------------------------------- |
| Debian or Ubuntu based             | [1. Debian package](#1-debian-package) |
| any other Linux                    | [2. Binary archive](#2-binary-archive) |
| building yourself (Go 1.26+)       | [3. From source](#3-from-source)    |

Release artifacts live on the
[releases page](https://github.com/foundriesio/ocicfs/releases). `<ver>` is
the release version (e.g. `0.1.0`), `<arch>` is `amd64` or `arm64`.

## 1. Debian package

Download `ocicfs_<ver>_<arch>.deb` and `ocicfs_<ver>_checksums.txt`, then:

```sh
sha256sum --check --ignore-missing ocicfs_<ver>_checksums.txt
sudo apt install ./ocicfs_<ver>_<arch>.deb
```

This installs `/usr/bin/ocicfs` plus bash and zsh completions.

- **Upgrade:** `sudo apt install ./ocicfs_<newver>_<arch>.deb`
- **Rollback:** `sudo apt install --allow-downgrades ./ocicfs_<oldver>_<arch>.deb`
- **Uninstall:** `sudo apt remove ocicfs`

## 2. Binary archive

Download `ocicfs_<ver>_linux_<arch>.tar.gz` and
`ocicfs_<ver>_checksums.txt`, then:

```sh
sha256sum --check --ignore-missing ocicfs_<ver>_checksums.txt
tar -xzf ocicfs_<ver>_linux_<arch>.tar.gz
sudo install -m 0755 ocicfs_<ver>_linux_<arch>/ocicfs /usr/local/bin/ocicfs
```

The archive also carries `LICENSE`, `README.md`, and this file.

- **Upgrade / rollback:** install the other version's binary the same way.
- **Uninstall:** `sudo rm /usr/local/bin/ocicfs`

## 3. From source

Needs Go 1.26 or newer. Build from a git checkout, not a source tarball, so
the binary reports its real version:

```sh
git clone https://github.com/foundriesio/ocicfs.git
cd ocicfs
make build
sudo make install
```

`make install` places the binary under `/usr/local/bin`; override with
`PREFIX=/usr` or `DESTDIR=<staging>` for packaging.

The default build uses fs-verity whenever the store's filesystem supports it.
To build a binary that refuses a filesystem without fs-verity, or one that
never uses fs-verity, pass one Go tag:

```sh
make build GOFLAGS=-tags=ocicfs_verity_required   # or ocicfs_verity_off
sudo make install GOFLAGS=-tags=ocicfs_verity_required
ocicfs version   # prints "verity: enabled (build: required)" on an fs-verity-capable store
```

`make install` builds again, so pass it the same `GOFLAGS`; without them it
installs a default (auto) binary. Pass at most one of the two tags; with both,
the build fails with `undefined: verity.Mode`.

See [docs/on-disk-layout.md](docs/on-disk-layout.md#the-fs-verity-mode)
for what each mode does.

- **Uninstall:** `sudo rm /usr/local/bin/ocicfs` (or your `PREFIX`)

## Check the installation

```sh
ocicfs --version
```

prints `ocicfs v<ver> (<commit>)` for a release build. `ocicfs version` adds
whether the binary runs the store with fs-verity (`enabled`) or without
(`disabled`), and the fs-verity mode it was built with; see
[the fs-verity mode](docs/on-disk-layout.md#the-fs-verity-mode) for what each
state means and when to build with `ocicfs_verity_required`:

```
ocicfs v<ver> (<commit>)
verity: enabled (build: auto)
```

## Runtime requirements

Installation works on any Linux; *using* ocicfs needs:

- **Kernel: Linux 6.6 or newer** for mounting — EROFS, loop devices, and
  overlayfs with `metacopy`, `redirect_dir` and data-only lower layers, plus
  fs-verity and overlayfs verity validation for a store with fs-verity. A vendor
  kernel carrying the same support also works; [docs/mount.md](docs/mount.md)
  lists the exact mainline commits.
- **A store filesystem with hardlinks.** fs-verity is optional and adds
  checks on every read. For fs-verity, ext4 needs 4096-byte blocks and
  `-O verity`; btrfs supports it out of the box. Without it, the default
  build checks content when written, not on later reads. The Go build tag
  `ocicfs_verity_required` makes fs-verity mandatory. See
  [the fs-verity mode](docs/on-disk-layout.md#the-fs-verity-mode).
  To add it on a host without it, see the
  [loopback ext4 setup](docs/mount.md#optional-add-fs-verity-with-a-loop-device).
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
gh attestation verify ocicfs_<ver>_linux_<arch>.tar.gz --repo foundriesio/ocicfs
```

An SPDX SBOM per binary (`ocicfs_<ver>_linux_<arch>.spdx.json`) is attached
to each release.

## First steps

Continue with the [quick start](README.md#quick-start) in the README: pull
an image, inspect it, mount it and unmount it. The README also indexes the
rest of the documentation.
