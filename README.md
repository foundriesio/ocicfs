# knarr

An OCI-native content delivery and mount engine for IoT/Edge devices. knarr
pulls standard OCI v1.1 images — fetching only the content a device does not
already have, from **zstd:chunked** layers — and serves them from a single
content-addressed store using **composefs + EROFS + fs-verity**, mounting a
filesystem tree with one `mount` syscall instead of unpacking and stacking
layers.

knarr provides **primitives, not lifecycle policy**: `pull`, `verify`, `mount`,
`unmount`, `hold`, `release`, and `gc` on the device, plus a publisher-side
`publish`. It does not execute containers and owns no version-management,
activation, rollback, or update-orchestration logic — those belong to the
upper-layer agent that drives knarr.

> **Status: early development.** The CLI surface and engine API are taking shape.
> `pull` from a local OCI layout is wired end to end; the remaining verbs are
> still being implemented.

## Project docs

- [Why knarr exists](docs/why.md): problem statement, requirements,
  constraints, and non-goals.
- [ADR 0001: High-level architecture](docs/adr/0001-high-level-architecture.md):
  the accepted architecture boundary and load-bearing technical decisions.
- [Pulling from a local OCI layout](docs/pull.md): how to run
  `knarr pull --from-dir`.
- [On-disk store layout](docs/on-disk-layout.md): what a pull writes under the
  data root.

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
make         # lint, vet, test, build (the default)
make test    # unit tests
make lint    # gofumpt + golangci-lint
make build   # stripped binary into bin/
```

## Contributing

Contributions are welcome. knarr is developed in the open with the goal of
homing under the [composefs](https://github.com/composefs) organization, and
follows that community's conventions. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
and the [Code of Conduct](CODE-OF-CONDUCT.md) first. All commits must be signed
off under the [Developer Certificate of Origin](https://developercertificate.org/)
(`git commit -s`), and pull requests must be opened and reviewed by a human.
Automated coding agents must additionally follow [AGENTS.md](AGENTS.md).

## License

knarr is licensed under the [Apache License 2.0](LICENSE).

Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
