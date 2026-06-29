# ADR 0001: High-level architecture

## Status

Accepted.

## Context

knarr targets IoT and edge systems that need OCI-native delivery without the
bandwidth, storage, and activation costs of the conventional pull, extract, and
overlay runtime flow. It must preserve compatibility with ordinary OCI tools
and registries while providing offline runtime behavior, air-gapped update
support, and end-to-end integrity.

## Decision

Build knarr as a Go library and CLI that provides content delivery and mount
primitives. Use standard OCI distribution on the wire, zstd:chunked metadata for
partial fetch, a sha256-keyed decompressed CAS on disk, and composefs + EROFS +
fs-verity for runtime mounts.

The architecture has two phases:

- Phase 1: the knarr engine and CLI provide `publish`, `pull`, `verify`,
  `mount`, `unmount`, `hold`, `release`, and `gc` primitives for prepared
  filesystem trees.
- Phase 2: `cfs-snapshotter` integrates the same knarr storage and mount
  primitives with containerd. The snapshotter is the container runtime adapter;
  knarr still does not execute containers.

## Architecture boundary

knarr owns image and filesystem materialization:

- resolve and fetch OCI content;
- parse OCI layer metadata (the zstd:chunked TOC for optimized pulls);
- verify descriptor and file digests;
- populate the local CAS;
- build or verify composefs/EROFS trees;
- mount and unmount prepared content;
- protect content through hold/release semantics — both live (mounted or
  prepared) content and the in-flight/partial content of a pull that has not
  finished materializing, so an interrupted update survives retries and
  garbage collection until the upper-layer agent commits or abandons it.

knarr does not own lifecycle policy:

- no deployment orchestration;
- no service supervision;
- no rollback policy;
- no container execution;
- no replacement for docker, containerd, kubelet, or an OTA agent.

## Load-bearing decisions

| Decision | Rationale |
|---|---|
| Standard OCI wire format, no new media types | Works with existing builders, registries, scanners, and signing; the zstd:chunked layout uses standard media types, not a protocol fork. |
| zstd:chunked for the optimized network pull | Its per-file TOC is what enables delta-only fetch, resumable transfer, and storage-fit preflight, so a network pull relies on it. Materializing an image does not require it — the local-directory sideload reads standard OCI layers, including plain `tar+gzip`, forgoing those optimizations. |
| Single decompressed copy in a sha256-keyed CAS | The compressed wire form is never persisted or buffered whole; files decompress straight into the CAS, so flash holds one copy per digest instead of compressed-plus-extracted. |
| composefs + EROFS + fs-verity on disk | Fast activation (one mount, no extract) and kernel-enforced runtime integrity. |
| Per-target pull lock over a no-replace CAS | The CAS is concurrency-safe by construction; the lock only blocks duplicate work on the same target, not unrelated pulls. |
| Progress as absolute store-reconciled bytes | knarr reports how much of the target is present (no progress journal); the caller frames the delta against its own baseline. |
| `mkcomposefs` for v1 generation | Uses the stable, packaged composefs path (builds composefs-c 1.0.8-compatible V1 EROFS) while keeping composefs-rs adoption open. |
| Digest-pinned verification before native trust | Keeps early phases small; the upper-layer agent supplies trust roots until native TUF/cosign lands. |
| Go implementation | Matches the surrounding container tooling and OCI client ecosystem. |

## Consequences

The wire protocol stays ordinary and device-specific optimization lives in the
client. The upper-layer agent boundary is explicit: knarr serves OTA systems,
scripts, or containerd, but never decides which version is active.

In return, knarr owns careful local state — resumable fetch, storage-fit
preflight, CAS population, integrity verification, mount tracking, and
garbage-collection protection (including a durable root that keeps an interrupted
pull's partial content alive until it is committed or abandoned). These belong in
knarr because they are part of materialization, not lifecycle policy.

## Rejected alternatives

- Custom registry protocol: rejected because it would block ordinary OCI
  infrastructure and air-gapped OCI Image Layout workflows.
- Custom layer media types for the core path: rejected because they reduce
  ecosystem compatibility.
- FUSE runtime filesystem: rejected for the core path because the target design
  is a kernel composefs mount.
- Persisting compressed blob cache: rejected because it breaks the
  single-decompressed-copy storage invariant.
- Container execution inside knarr: rejected because runtimes and supervisors
  already own execution policy.
- Custom signing envelope: rejected because trust should compose with TUF,
  cosign, OCI descriptors, and the upper-layer agent.

## References

- [Why knarr exists](../why.md)
- [README](../../README.md)
