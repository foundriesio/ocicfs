# Why ocicfs exists

ocicfs is an OCI-native content delivery and mount engine for IoT and edge
devices. It exists because the mainstream container stack was designed for
data-center assumptions: reliable networks, plentiful CPU and memory, large
storage, and always-reachable registries.

Edge deployments invert those assumptions. Devices may have intermittent
connectivity, single-digit-Mbps links, small flash devices, ARM-class CPUs,
multi-year service lives, and OTA as the only practical update channel. The
standard pull, extract, and overlay flow still works there, but it spends
bandwidth, storage, and boot time that many products cannot afford.

## Requirements

| Requirement | Why it matters |
|---|---|
| OCI-compliant packaging | Use existing build, registry, signing, and inspection tools. |
| Standard registry distribution | Avoid custom server-side software and keep CDN/proxy compatibility. |
| Resumable transfers | Pulls must survive network drops and device reboots. |
| Delta-only fetch | A small file-level change must not force a full image download. |
| Compressed transfer | Bandwidth is metered and often slow. |
| Minimal on-disk footprint | Flash is small; duplicate content shortens device lifetime. |
| Storage-fit preflight | Flash is small; an update that will not fit is refused before any payload is written. |
| Compute efficiency | Low-power CPUs should not untar and rehash whole images unnecessarily. |
| Fast activation | Restart paths should mount a prepared tree, not extract layers. |
| Fully offline runtime | After pull, running software must not depend on a registry. |
| Air-gapped updates | Removable-media updates need the same integrity story as online pulls. |
| End-to-end integrity | Field devices must detect tampered content at rest and at runtime. |
| Supply-chain trust | Provenance must work for online and offline delivery. |

These are requirements, not a feature list. Three are met only in part
today: the storage-fit check runs only on delta pulls (see
[pull.md](pull.md#storage-and-source-requirements)); an interrupted pull
does not resume a partial download, though a retry reuses the files
already stored; and end-to-end integrity on read requires fs-verity (see
[the fs-verity mode](on-disk-layout.md#the-fs-verity-mode)).

## Constraints

ocicfs is deliberately constrained so it can fit into existing OSS and device
ecosystems:

- Wire format is standard OCI v1.1, no new media types.
- zstd:chunked is the wire format for the optimized delta pull: its per-file
  TOC is what makes delta-only fetch and storage-fit preflight possible. A plain
  pull, from a registry or a sideload, also reads standard gzip, zstd, and
  uncompressed layers whole, forgoing those optimizations, and a delta pull
  falls back to whole-layer reads where ranges do not pay.
- Distribution uses ordinary OCI registries or OCI Image Layout for air-gapped
  transport.
- On-disk runtime state uses a single decompressed content-addressed store,
  composefs, EROFS, and optional fs-verity.
- The v1 generator path uses `mkcomposefs`; future composefs-rs adoption should
  be gated by compatibility and upstream stability.
- The device runtime path must not require a custom registry, custom signing
  envelope, always-on FUSE daemon, or duplicated compressed blob cache.

## Non-goals

ocicfs provides primitives, not lifecycle policy. It does not own deployment
decisions, rollout state, service supervision, rollback policy, or container
execution.

Specifically, ocicfs does not provide `run`, `deploy`, `update`, `stop`, or
`logs`-style lifecycle behavior. Those decisions belong to the upper-layer
agent or runtime that calls ocicfs. In the container runtime integration path,
docker/containerd uses `cfs-snapshotter` as the adapter; ocicfs itself prepares
and mounts content.

## Why existing tools are not enough

Standard runtimes download whole layers, extract them, and stack overlayfs —
simple and interoperable, but costly in bandwidth and storage on small devices.
Lazy-pull filesystems improve startup but often need a daemon or a custom format.
OS-image systems solve a related OTA problem but target whole host operating
systems, not OCI-native application content. composefs gives the right integrity
and mount substrate but not the pull, resume, air-gap, trust, and hold/release
primitives a device update agent needs.

ocicfs fills that composition gap: it keeps the OCI ecosystem on the wire,
stores only verified decompressed content on disk, mounts prepared trees through
composefs, and leaves lifecycle policy to the layer that already owns update
decisions.
