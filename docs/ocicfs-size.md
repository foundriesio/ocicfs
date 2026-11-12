# Estimating image storage (`ocicfs-size`)

`ocicfs-size` estimates how much disk space a set of OCI images needs under
four storage models. It also estimates update downloads and extra space
needed while old and new images are both held. Run it on a developer or CI
machine, not on the device.
Results for real updates of public images are in
[ocicfs-size-results.md](ocicfs-size-results.md).

## Build and run

Build the separate binary from the ocicfs repository root. It is not
included in release archives or the Debian package.

```sh
make ocicfs-size
bin/ocicfs-size images docker.io/library/nginx:1.29.8
```

Pass multiple references to size them together, or use `@FILE` for one
reference per line (`@-` reads stdin). The platform defaults to the host's
Linux architecture; use `--platform` when sizing for another device.

The tool reads manifests and layers using anonymous access or Docker
credentials. It reads each layer once into a cache, by default at
`$XDG_CACHE_HOME/ocicfs-size`. Sizes are calculated by formula: it does not
mount images, run mkfs, or write a store.

Use `--details` for the storage projection, assumptions, exclusions and
pins, or `--json` for the report as JSON. See `images --help` for options.

## Read the storage estimate

```text
STORAGE ESTIMATE · 1 image · linux/amd64
- docker.io/library/nginx:1.29.8@sha256:ab15d428b6a7121511a38221591a41f835933ac16f996fafd102d128a0fa20f7

OCI layers:       7 references · 7 unique blobs
Compressed size:  60.0 MiB total · 60.0 MiB unique

Estimated disk usage (formula-based): ext4 · 4 KiB blocks · no compression · inline max size 64 B (File CAS/image)
─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
                                                          fs-verity off                         fs-verity on
Storage model    Stored files       vs Overlay          Size        vs Overlay      Size (vs own off)      vs Overlay off
─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
Overlay                 3,729                —     166.2 MiB                 —                      —                   —
EROFS/layer                 7  -3,722 (-99.8%)     163.6 MiB  -2.6 MiB (-1.5%)     165.0 MiB (+0.8%)    -1.2 MiB (-0.7%)
File CAS/image          3,403    -326 (-8.7%)      162.0 MiB  -4.2 MiB (-2.5%)     181.2 MiB (+11.8%)  +15.0 MiB (+9.0%)
File CAS/layer          3,536    -193 (-5.2%)      163.3 MiB  -2.9 MiB (-1.8%)     183.0 MiB (+12.1%)  +16.8 MiB (+10.1%)
```

Compressed size describes registry blobs; use **Size** for estimated disk
usage. Repeated blobs count once in the unique total.

- **Overlay:** containerd's overlayfs snapshotter, Docker's default for its
  containerd image store. Each layer is unpacked as files, without fs-verity.
- **EROFS/layer:** one EROFS image file per layer, using containerd's default
  EROFS snapshotter configuration (containerd v2.4.2, erofs-utils 1.9.4).
- **File CAS/image:** ocicfs / cfs-snapshotter. File contents are shared
  across images and layers in a content-addressed object store, with one
  composefs metadata image per image. Files up to 64 B are inline by default.
- **File CAS/layer:** an experimental prototype on unreleased containerd,
  with per-layer metadata-only EROFS over a shared object store. It requires
  fs-verity; its “off” figures do not describe a runnable configuration.

The fs-verity “on” sizes include per-file Merkle trees. In ocicfs, fs-verity
is optional; auto mode uses it when the filesystem supports it.

## How the sizes are calculated

Each size is a sum of whole 4 KiB ext4 blocks:

```text
size = file data + images + OCI metadata + fs-verity + host metadata

file data      each stored regular file, rounded up to 4 KiB; hardlinks
               count once
images         each EROFS or composefs image, rounded up to 4 KiB, less the
               file data counted above
OCI metadata   File CAS/image only: each image's index, manifest and config
fs-verity      per sealed file: one descriptor block, plus a Merkle tree of
               about 1/127 of the file in whole blocks; files up to 4 KiB
               have no tree
host metadata  ext4 directory and shared xattr blocks, long symlinks, and the
               store's small files: bindings, references, saved scan records
```

| Model | File data | Images | Sealed with fs-verity |
|---|---|---|---|
| Overlay | every file of every layer snapshot | none | nothing |
| EROFS/layer | every file of every layer snapshot, inside its image | one `layer.erofs` per layer snapshot | each `layer.erofs` |
| File CAS/image | one object per distinct content above the inline max that an image shows, shared by all images | one composefs image per distinct image, holding the inline files | every object, image and OCI metadata file |
| File CAS/layer | one object per distinct non-empty content of any layer, hidden or not, shared by all layers | one metadata-only `layer.erofs` per layer snapshot | every object and image |

Images that share base layers share their layer snapshots: a snapshot is
identified by its layer and the layers below it (its ChainID).

Every sealed file costs at least one block, so fs-verity adds under 1% to
EROFS/layer, which seals a few large images, and about 12% to File CAS/image
for nginx above, which seals about 3,400 mostly small files.

Image sizes and directory blocks are not closed formulas: the tool lays out
each image and directory the way mkfs.erofs, mkcomposefs 1.0.8 and ext4 do,
and counts the blocks. `--host-metadata exclude` drops the host metadata
term.

## Scope and accuracy

The model assumes ext4, 4 KiB blocks and no compression. Host filesystem
metadata is included by default. Inodes are not counted: ext4 allocates
them when the filesystem is made. The estimate excludes containerd's
compressed content cache and `metadata.db`, writable container layers,
application data, extraction workspace and garbage-collection delay.

Measurements against real ext4 stores on an amd64 VM and an arm64 board
matched Overlay and EROFS/layer to the byte. File CAS/image was within
0.11% after garbage collection. On boards with little RAM, ext4
fragmentation adds some space that the model does not count.

## Estimate an update

Compare the image sets held before and after an update:

```sh
bin/ocicfs-size update --from docker.io/library/nginx:1.29.7 --to docker.io/library/nginx:1.29.8
```

```text
UPDATE ESTIMATE · nginx:1.29.7 → nginx:1.29.8 · linux/amd64 · after cleanup: nginx:1.29.8 only
- from: docker.io/library/nginx:1.29.7 @5a28f02fc9f6
- to:   docker.io/library/nginx:1.29.8 @ab15d428b6a7

Download (compressed bytes the device fetches):
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
Transfer path                           Download  What it fetches                                Feeds
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
containerd pull                         60.0 MiB  7 of 7 layers (none shared with nginx:1.29.7)  every storage model
ocicfs pull                             60.1 MiB  7 of 7 layers                                  File CAS/image
ocicfs pull --delta                     60.1 MiB  7 of 7 layers whole: no zstd:chunked layers    File CAS/image
ocicfs pull --delta, as zstd:chunked   ~12.6 MiB  modeled: 7 TOCs + 15 ranges (-79.1%)           File CAS/image

Disk usage (fs-verity on; Overlay has none):
───────────────────────────────────────────────────────────────────────────────────────────
Storage model       before       needed           vs Overlay      after          vs Overlay
───────────────────────────────────────────────────────────────────────────────────────────
Overlay          166.2 MiB   +166.2 MiB                    -  166.2 MiB                   -
EROFS/layer      165.0 MiB   +165.0 MiB    -1.2 MiB (-0.7%)   165.0 MiB   -1.2 MiB (-0.7%)
File CAS/image   181.2 MiB    +30.6 MiB  -135.6 MiB (-81.6%)  181.2 MiB  +15.0 MiB (+9.0%)
File CAS/layer   183.0 MiB    +31.0 MiB  -135.2 MiB (-81.4%)  183.0 MiB  +16.8 MiB (+10.1%)
…
```

**Before** is the current storage; **needed** is extra free space while
both sets are held; **after** is storage after cleanup. `--rollback` keeps
the old images after cleanup. Each is the size above, taken over a set of
images:

```text
before = size(from)
needed = size(from and to together) − size(from)
after  = size(to), or size(from and to together) with --rollback
```

`containerd pull` fetches whole layers not already held. `ocicfs pull`
fetches whole layers it has not already saved; `--delta` fetches the table
of contents and changed file byte ranges for zstd:chunked layers, and
other layers whole. Every path also fetches each new image's index,
manifest and config.

The “as zstd:chunked” row models republishing images in that format; it is
not the download for the images as published. Its per-file compression
estimates were 4.5% below measured downloads for nginx and 6.5% for python,
so treat it as a slight underestimate. See `update --help` for options.
