<!-- SPDX-License-Identifier: Apache-2.0 -->

# CFS Snapshotter: End-to-End Integration

Integrating ocicfs and [cfs-snapshotter][cfs] involves decisions at four stages of the image lifecycle:

1. Publishing: building and packaging images.
2. Host setup: configuring the host OS, ocicfs, and cfs-snapshotter.
3. Preloading: making images available on a host/device before first use or startup, without needing to pull them from a registry.
4. Updating: updating images and rolling back.

This guide lists these decisions and their trade-offs, and links to the documentation that covers each step.

For one path through all four stages as runnable scripts, see the [integration example](../examples/integration/README.md).

## Summary

Most goals need a decision at more than one stage. Pick the goals you want and apply every cell in their rows.

| If you want | Publishing | Host setup | Preloading and updating |
|---|---|---|---|
| Delta pulls: download only the changed files instead of whole layers | [`zstd:chunked` layers](#publishing) | [Preloaded mode](#hostdevice-setup) | [`ocicfs pull --delta`](#updating), with the previous version still in the store as the base |
| Storage usage optimization: no layer archives kept in containerd's content store | | [Preloaded mode](#hostdevice-setup) | [`ocicfs pull`](#preloading), then register the image with a metadata-only runtime pull (`docker pull`), which needs the registry to be reachable |
| Publisher-attested merged rootfs (all image layers combined), verified on every use | [Sealed image](#publishing) | [fs-verity](#hostdevice-setup) | Preload and update by [pinned image digest](#publishing) |
| File content checked on every read | | [fs-verity](#hostdevice-setup) | |
| First use without a registry | [Image archive](#preloading) | | [Import and load the archive](#preloading) |

Files are stored once and shared across layers, images, and versions whichever options you choose.

## Publishing

Build and push images with your existing tools, then:

1. [Convert images to `zstd:chunked`][layer-format] if delta pulls are required or desired.
2. [Seal images][seal] if you want the rootfs verified against a publisher-attested digest each time an image is mounted. Combined with a pinned image digest, this verifies image authenticity on every use, not only at pull time. This needs fs-verity on the host: without it, only the image metadata is checked at mount, and file contents are not checked when read.

Conversion and sealing each change the image digest. Deploy the final one.

## Host/Device Setup

**Prerequisite**: [cfs-snapshotter must be installed][cfs-install] and [connected to containerd and Docker][cfs-connect] on the host before use. Install [ocicfs](../INSTALL.md) as well if you preload images.

1. [Enable preloaded mode][preloaded-mode] if you want to avoid the additional storage used by containerd's content store, which keeps image layer archives and metadata alongside the ocicfs store. Delta pulls also need this mode. In the default mode, `docker pull` and `docker load` work as usual and no preloading step is needed.
2. [Enable fs-verity][fs-verity] if you want image integrity checked when the rootfs is mounted and its files are read, so that damaged image files are detected. Sealed images need it for the seal to cover file contents.

## Preloading

Decide when and how to preload container images onto the host/device.

**When to preload**

1. During system image creation or immediately afterward. Not validated yet; until it is, ship an image archive in the system image and load it on first boot.
2. During host/device provisioning in manufacturing.
3. On the host/device's first boot.

**How to preload**

1. [Pull images directly from a registry][registry-pull] with `ocicfs pull`, then [run `docker pull`][preload-run] to register them with the runtime. The image is already in the store, so `docker pull` downloads only the manifest and config, not the layers. Docker 29.9 or newer needs [`"lazy-pull": true`][lazy-pull] for this.
2. [Save images to an OCI archive][copy], [import it][from-archive] with `ocicfs pull`, then [load it into the runtime][offline-load]. `docker load` copies the archive's layers into containerd's content store, so this path does not save that storage. To save it, register the imported image with `docker pull` instead of loading it. That downloads the manifest and config, so the registry must be reachable at that moment and the first start is not fully offline.

## Updating

The steps depend on the mode chosen in [Host/Device Setup](#hostdevice-setup). In both, switching the workload, checking it, and deciding to roll back belong to your update agent.

**Preloaded mode: `ocicfs pull [--delta]`**

1. Keep the running version in the store: a delta pull uses it as its base, and it stays available for rollback. A digest-pinned reference already keeps it. If you deploy by tag, [hold it][delta] first, because pulling a moved tag repoints the reference.
2. Pull the new version with a [delta pull][delta] or a [plain pull][registry-pull].
3. Register it with the runtime (`docker pull` or `docker load`) as in [Preloading](#preloading).
4. Switch the workload to the new version and check it.
5. Roll back to the previous version, or drop it: remove its containers and its runtime image (`docker image rm`), release your hold if you took one, [remove its reference][remove], and [reclaim the space][gc].

**Default mode: `docker pull`**

1. [Pull the new version through the runtime][runtime-pull] (`docker pull`). It downloads whole layers, with no file-level delta; files already in the store are not stored twice.
2. Switch the workload to the new version and check it.
3. Roll back to the previous version, or remove it with `docker image rm`. The snapshotter then releases its hold and reclaims the space on its own.

Use [ocicfs-size](ocicfs-size.md) to estimate the storage an update needs. See [image retention and garbage collection][retention] for how runtime snapshots and holds interact.

[cfs]: https://github.com/foundriesio/cfs-snapshotter
[cfs-install]: https://github.com/foundriesio/cfs-snapshotter/blob/main/INSTALL.md
[cfs-connect]: https://github.com/foundriesio/cfs-snapshotter/blob/main/INSTALL.md#connect-containerd-and-docker
[fs-verity]: https://github.com/foundriesio/cfs-snapshotter/blob/main/INSTALL.md#fs-verity-modes
[preloaded-mode]: https://github.com/foundriesio/cfs-snapshotter/blob/main/docs/running.md#1-select-preloaded-mode
[lazy-pull]: https://github.com/foundriesio/cfs-snapshotter/blob/main/INSTALL.md#2-connect-docker-if-used
[runtime-pull]: https://github.com/foundriesio/cfs-snapshotter/blob/main/docs/running.md#pulling-without-staging-first
[preload-run]: https://github.com/foundriesio/cfs-snapshotter/blob/main/docs/running.md#preload-pull-and-run
[offline-load]: https://github.com/foundriesio/cfs-snapshotter/blob/main/docs/running.md#loading-images-offline-docker-load
[retention]: https://github.com/foundriesio/cfs-snapshotter/blob/main/docs/running.md#image-retention-and-garbage-collection
[layer-format]: pull.md#layer-format-and-verification
[seal]: seal.md
[registry-pull]: pull.md#registry-pull
[from-archive]: pull.md#from-a-docker-save-or-skopeo-archive
[copy]: pull.md#writing-an-archive-with-ocicfs-copy
[delta]: pull.md#delta-pull
[remove]: ls-remove.md
[gc]: gc.md
