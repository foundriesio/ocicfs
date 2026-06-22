# knarr

An OCI-native content delivery and mount engine for IoT/Edge devices. knarr
pulls standard OCI v1.1 images in **zstd:chunked** format — fetching only the
content a device does not already have — and serves them from a single
content-addressed store using **composefs + EROFS + fs-verity**, mounting a
filesystem tree with one `mount` syscall instead of unpacking and stacking
layers.

knarr provides **primitives, not lifecycle policy**: `pull`, `verify`, `mount`,
`unmount`, `hold`, `release`, and `gc` on the device, plus a publisher-side
`publish`. It does not execute containers and owns no version-management,
activation, rollback, or update-orchestration logic — those belong to the
upper-layer agent that drives knarr.

> **Status: early development.** This repository currently contains only project
> governance and contribution scaffolding — there is no implementation yet.
> Documentation for building, testing, and usage will be added as the code lands.

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
