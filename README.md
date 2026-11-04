# ocicfs

An OCI-native content delivery and mount engine for IoT/Edge devices. ocicfs
pulls standard OCI v1.1 images — fetching only the content a device does not
already have, from **zstd:chunked** layers — and serves them from a single
content-addressed store using **composefs + EROFS + fs-verity**, mounting a
filesystem tree with one `mount` syscall instead of unpacking and stacking
layers.

ocicfs provides **primitives, not lifecycle policy**: `pull`, `verify`, `mount`,
`unmount`, `hold`, `release`, and `gc` on the device, plus the publisher-side
`publish` and `copy`. It does not execute containers and owns no version-management,
activation, rollback, or update-orchestration logic — those belong to the
upper-layer agent that drives ocicfs. Containers run under containerd or Docker
through [cfs-snapshotter](https://github.com/foundriesio/cfs-snapshotter), which
serves images from an ocicfs store.

> **Status: early development.** The device-side verbs are wired end to
> end: `pull` from an OCI registry (digest-pinned or tagged) or a local OCI
> layout, with fail-closed verification of sealed images and, with
> `--delta`, per-file fetch from the zstd:chunked TOC (see
> [docs/pull.md](docs/pull.md)); read-only, writable (`--rw`), and
> metadata-only `mount`; `unmount`, `ls`, `inspect`, `hold`/`release`,
> `remove`, and `gc`; plus the publisher-side `seal` and `copy`, which writes
> registry images to an archive for offline delivery. The publisher-side
> `publish` is still to come.

## Install

[INSTALL.md](INSTALL.md) covers the Debian package, the release archives, and
building from source.

## Project docs

- [Why ocicfs exists](docs/why.md): problem statement, requirements,
  constraints, and non-goals.
- [ADR 0001: High-level architecture](docs/adr/0001-high-level-architecture.md):
  the accepted architecture boundary and load-bearing technical decisions.
- [ADR 0002: Image identity](docs/adr/0002-image-identity.md): the platform
  manifest digest as the one key for a stored image.
- [Pulling images](docs/pull.md): registry pull and the local sideload from an
  OCI layout directory (`--from-dir`) or an archive (`--from-tar`) written by
  `ocicfs copy`, `docker save` or skopeo.
- [Sealing an image](docs/seal.md): the publisher-side `ocicfs seal`, which
  records the composefs integrity digests a pull verifies.
- [Mounting a pulled image](docs/mount.md): read-only composefs mounts and
  metadata-only mounts for overlay consumers.
- [Inspecting a local image](docs/inspect.md): image identity, the composefs
  binding, and the per-layer view.
- [Listing and forgetting local images](docs/ls-remove.md): `ocicfs ls` and
  `ocicfs remove`.
- [Reclaiming store space](docs/gc.md): `ocicfs gc` and the wanted/in-use
  model that decides what it keeps.
- [On-disk store layout](docs/on-disk-layout.md): what a pull writes under the
  data root.
- [Hands-on storage walkthrough](docs/storage-walkthrough.md): build and verify
  a composefs image with the upstream tools, by hand.

## Development

Development runs in a pinned dev container (`.devcontainer/`, built from the
`Containerfile`) so the Go toolchain and linters match CI. It needs Docker and
the [`devcontainer` CLI](https://github.com/devcontainers/cli):

```sh
./devshell.sh        # build/start the container and open a shell (removed on exit)
./devshell.sh exec   # open another shell in the running container (left running)
```

Inside the shell, the make targets behave exactly as they do in CI:

```sh
make         # stripped binary into bin/ (the default)
make check   # lint, vet, unit tests
make test    # unit tests
make lint    # gofumpt + golangci-lint
make smoke   # pull --from-dir, mount, unmount, remove
```

Where each target runs:

| Target | Dev host | Dev container |
|---|---|---|
| `make`, `build`, `size`, `clean` | yes | yes |
| `install` | yes, as root or with a writable `PREFIX` or `DESTDIR` | yes |
| `check`, `test`, `vet`, `lint`, `fmt` | yes, with Go and golangci-lint installed; tests that need root, fs-verity or mkcomposefs skip themselves | yes, full coverage as in CI |
| `smoke` | no: needs a privileged environment with fs-verity and mkcomposefs | yes |
| `deb`, `deb-lint`, `deb-test` | yes, with Docker: they build in their own container | not needed |

## Contributing

Contributions are welcome. ocicfs is developed in the open and follows the
conventions of the composefs and containerd communities. Please read
[CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE-OF-CONDUCT.md)
first. All commits must be signed
off under the [Developer Certificate of Origin](https://developercertificate.org/)
(`git commit -s`), and pull requests must be opened and reviewed by a human.
Automated coding agents must additionally follow [AGENTS.md](AGENTS.md).

## License

ocicfs is licensed under the [Apache License 2.0](LICENSE).

Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
