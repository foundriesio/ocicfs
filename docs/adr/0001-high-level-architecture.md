# ADR 0001: High-level architecture

## Status

Accepted.

## Context

ocicfs targets IoT and edge systems that need OCI-native delivery without the
bandwidth, storage, and activation costs of the conventional pull, extract, and
overlay runtime flow. It must preserve compatibility with ordinary OCI tools
and registries while providing offline runtime behavior, air-gapped update
support, and end-to-end integrity.

## Decision

Build ocicfs as a Go library and CLI that provides content delivery and mount
primitives. Use standard OCI distribution on the wire, zstd:chunked metadata for
partial fetch, a sha256-keyed decompressed CAS on disk, and composefs + EROFS +
fs-verity for runtime mounts.

The architecture has two phases:

- Phase 1: the ocicfs engine and CLI provide `publish`, `pull`, `verify`,
  `mount`, `unmount`, `hold`, `release`, and `gc` primitives for prepared
  filesystem trees.
- Phase 2: `cfs-snapshotter` integrates the same ocicfs storage and mount
  primitives with containerd. The snapshotter is the container runtime adapter;
  ocicfs still does not execute containers.

## Architecture boundary

ocicfs owns image and filesystem materialization:

- resolve and fetch OCI content;
- parse OCI layer metadata (the zstd:chunked TOC for optimized pulls);
- verify descriptor and file digests;
- populate the local CAS;
- build or verify composefs/EROFS trees;
- mount and unmount prepared content;
- protect content through hold/release semantics and mount tracking, and
  keep garbage collection out while a pull runs; an interrupted pull's objects
  stay in the store until the next collection, and a retry reuses them.

ocicfs does not own lifecycle policy:

- no deployment orchestration;
- no service supervision;
- no rollback policy;
- no container execution;
- no replacement for docker, containerd, kubelet, or an OTA agent.

## Load-bearing decisions

| Decision | Rationale |
|---|---|
| Standard OCI wire format, no new media types | Works with existing builders, registries, scanners, and signing; the zstd:chunked layout uses standard media types, not a protocol fork. |
| zstd:chunked for the optimized network pull | Its per-file TOC is what enables delta-only fetch, resumable transfer, and storage-fit preflight, so the delta pull relies on it. Materializing an image does not require it — registry pulls and sideloads read standard OCI layers, including plain `tar+gzip`, whole, forgoing those optimizations. |
| Single decompressed copy in a sha256-keyed CAS | The compressed wire form is never persisted; files decompress straight into the CAS, so flash holds one copy per digest instead of compressed-plus-extracted. Exception: a delta pull whose source answers the TOC range request with the whole blob spools it to a store temp file until the pull ends. |
| composefs + EROFS + fs-verity on disk | Fast activation (the image's EROFS metadata served through one read-only overlay with the CAS as a data-only lower; no extract, no per-layer stack) and kernel-enforced runtime integrity. |
| Shared store lock over a no-replace CAS | The CAS is concurrency-safe by construction, so pulls and mounts share the store lock and only garbage collection takes it exclusively. Concurrent pulls of the same target are safe but duplicate work. |
| Progress as absolute store-reconciled bytes (not yet implemented) | ocicfs reports how much of the target is present (no progress journal); the caller frames the delta against its own baseline. |
| `mkcomposefs` for v1 generation | Uses the stable, packaged composefs path (builds composefs-c 1.0.8-compatible V1 EROFS) while keeping composefs-rs adoption open. |
| Digest-pinned verification before native trust | Keeps early phases small; the upper-layer agent supplies trust roots until native TUF/cosign lands. |
| Go implementation | Matches the surrounding container tooling and OCI client ecosystem. |

## Consequences

The wire protocol stays ordinary and device-specific optimization lives in the
client. The upper-layer agent boundary is explicit: ocicfs serves OTA systems,
scripts, or containerd, but never decides which version is active.

In return, ocicfs owns careful local state — reuse of already-stored content
on retry, storage-fit preflight, CAS population, integrity verification, mount
tracking, and garbage-collection protection. These belong in
ocicfs because they are part of materialization, not lifecycle policy.

## Rejected alternatives

- Custom registry protocol: rejected because it would block ordinary OCI
  infrastructure and air-gapped OCI Image Layout workflows.
- Custom layer media types for the core path: rejected because they reduce
  ecosystem compatibility.
- FUSE runtime filesystem: rejected for the core path because the target design
  is a kernel composefs mount.
- Persisting compressed blob cache: rejected because it breaks the
  single-decompressed-copy storage invariant.
- Container execution inside ocicfs: rejected because runtimes and supervisors
  already own execution policy.
- Custom signing envelope: rejected because trust should compose with TUF,
  cosign, OCI descriptors, and the upper-layer agent.

## References

- [Why ocicfs exists](../why.md)
- [README](../../README.md)
