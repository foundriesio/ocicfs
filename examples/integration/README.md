<!-- SPDX-License-Identifier: Apache-2.0 -->

# Integration example

One path through the [integration guide](../../docs/integration.md), as scripts to read and copy from. A sealed `zstd:chunked` image is preloaded from an archive and updated with a delta pull. The update is then either rolled back or finalized. The host runs cfs-snapshotter in preloaded mode with fs-verity.

| Script | Guide section | What it does |
|---|---|---|
| [`0-prerequisites.sh`](0-prerequisites.sh) | | Installs Docker 29, containerd 2.1 or newer, `mkcomposefs`, a recent skopeo, and ocicfs |
| [`1-publish.sh`](1-publish.sh) | [Publishing](../../docs/integration.md#publishing) | Builds and pushes two versions of the [example image](Dockerfile), converts them to `zstd:chunked`, seals them, and saves version 1 to an archive |
| [`2-host-setup.sh`](2-host-setup.sh) | [Host/Device Setup](../../docs/integration.md#hostdevice-setup) | Installs cfs-snapshotter, connects containerd and Docker, enables preloaded mode and fs-verity |
| [`3-preload.sh`](3-preload.sh) | [Preloading](../../docs/integration.md#preloading) | Imports version 1 from the archive with no network, checks that a file present in two layers is stored once, registers the image with Docker, and runs it |
| [`4-update.sh`](4-update.sh) | [Updating](../../docs/integration.md#updating), steps 1 to 4 | Pulls version 2 as a delta, registers it with Docker, stops version 1 and starts version 2 |
| [`5-rollback.sh`](5-rollback.sh) | [Updating](../../docs/integration.md#updating), step 5 | If the new version is not good: stops it, starts version 1 again, then removes version 2 and reclaims its space |
| [`5-finalize.sh`](5-finalize.sh) | [Updating](../../docs/integration.md#updating), step 5 | If the new version is good: removes the stopped version 1 and reclaims its space |

Numbered comments refer to the steps of the linked guide section. Comments that start with "Check" mark what the example verifies. [`check.sh`](check.sh) checks the running container after each switch: it runs the pinned digest, its root filesystem requires fs-verity, the publisher's seal matches the image in the store, and containerd holds no copy of its layers.

Each script stops at the first command that fails, a check included, and prints every command as it runs.

The [Integration example workflow](../../.github/workflows/integration-example.yml) runs the scripts on a GitHub-hosted Ubuntu 24.04 runner, against a private image on ghcr.io. To cover both outcomes, it updates, rolls back, updates again, and finalizes.

## Running it yourself

Use a disposable Ubuntu 24.04 machine. The scripts replace Docker and containerd, switch Docker's storage to the snapshotter, and mount a loopback filesystem at `/var/lib/ocicfs`.

1. Set the versions that the [workflow](../../.github/workflows/integration-example.yml) sets in `env`.
2. Set `IMAGE` to a private repository you can push to, and log in to its registry with `docker login`.
3. Run the scripts in order from the repository root. After `4-update.sh`, run either `5-rollback.sh` or `5-finalize.sh`.
