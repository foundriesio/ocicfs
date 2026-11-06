# Mounting a pulled image (`ocicfs mount`)

`ocicfs mount` exposes an already-pulled image as a read-only composefs
filesystem. It does not pull, rebuild, or execute anything; it reads the local
binding and object store produced by `ocicfs pull`. The reference may pin a
digest (`name@sha256:<hex>` or a bare `sha256:<hex>`) or be any ref string a
previous pull recorded, such as a tag: an unpinned ref resolves locally —
never over the network — to the identity its most recent pull recorded, and a
pinned index digest resolves to the manifest that index offers for the host
platform. Every verification keys on that identity digest.

## Prerequisites

- A store containing the pulled image, normally on an **fs-verity-capable
  filesystem**. A build running without fs-verity (`ocicfs version` prints
  `verity: disabled`) mounts without it (see
  [on-disk-layout.md](on-disk-layout.md#the-fs-verity-mode)).
- A target directory that already exists and carries no mount of its own:
  mounts never stack, so a target that is already a mountpoint is refused
  until it is unmounted.
- Kernel support for EROFS and overlayfs with data-only lower layers
  (mainline commit 37ebf056d6cf, Linux 6.5) and, for a store with fs-verity,
  fs-verity and its validation of lower data for `verity=require` (the Linux
  6.6 series through mainline commit 184996e92e86, "ovl: Validate verity
  xattr when resolving lowerdata", the last patch this read-only path
  requires) — so Linux 6.6, or a vendor kernel carrying all of the above. Below that floor the mount fails with the
  overlay mount's error; there is no fallback.

### No fs-verity filesystem? Create one on a loop device

The default build stores objects without fs-verity on a filesystem that cannot
enable it, and a `required` build refuses it. To keep fs-verity on
such a host, back the store with a loopback ext4 that supports it (fs-verity needs a 4096-byte block size):

```sh
sudo fallocate -l 2G /var/lib/ocicfs.ext4
sudo mkfs.ext4 -F -b 4096 -O verity -m 0 /var/lib/ocicfs.ext4
sudo mkdir -p /var/lib/ocicfs
sudo mount -o loop /var/lib/ocicfs.ext4 /var/lib/ocicfs
```

## 1. Pull a digest-pinned image

See [pull.md](pull.md) for creating a local OCI layout and pulling it into the
store:

```sh
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
ocicfs pull --from-dir ./layout "app@$digest"
```

## 2. Create the mount target

ocicfs does not create or clean the target directory:

```sh
sudo mkdir -p /mnt/app
```

## 3. Mount read-only

```sh
sudo ocicfs mount "app@$digest" /mnt/app
```

Before anything else, mount resolves and digest-verifies the stored metadata
chain in `objects/sha256` in a single pass — the index when the reference pins
one, the platform manifest, and its config. Only then does it read the
host-platform binding from `images/` for the verified identity and pick the
fs-verity digest to enforce: the manifest's merged-image sealing annotation
when one is present, the digest the binding recorded at pull otherwise. It
re-measures the referenced composefs image object against that value on the
very file descriptor it attaches to a read-only loop device, mounts the
image's EROFS metadata on a private scratch directory under `/run/ocicfs`, and
assembles the mount at the target as a single read-only overlay with the
store's object directory as a data-only lower layer and fs-verity required on
every backing object. The scratch mountpoint is lazily detached before the
mount returns — the overlay keeps the metadata, the loop device, and the
image's in-use lock alive until the target is unmounted. The mount fails if
the metadata chain or the image object is missing, unmeasurable, or measured
differently from the enforced digest. Without fs-verity (`verity: disabled`)
the image's fs-verity digest is computed from that descriptor's bytes instead
of measured, and the overlay is assembled without the `verity=` option.

The mounted tree is read-only:

```sh
find /mnt/app -maxdepth 2 -type f
cat /mnt/app/etc/config.txt
```

## 4. Unmount

```sh
sudo ocicfs unmount /mnt/app
```

`unmount` tears down the mount at the target path. It does not remove the target
directory or delete store content.

## Metadata-only mounts (`--metadata-only`)

A consumer that assembles its own writable overlay (a containerd snapshotter,
or a script-driven agent) mounts only the image's verified EROFS metadata and
folds the store's object directory into that overlay as a data-only lower:

```sh
sudo ocicfs mount --metadata-only "app@$digest" /mnt/app-meta
sudo mount -t overlay overlay -o \
  "lowerdir=/mnt/app-meta::/var/lib/ocicfs/objects/fsverity-sha256-12,upperdir=$upper,workdir=$work,redirect_dir=on,metacopy=on,verity=require" \
  /mnt/app
```

The metadata mount alone serves no file data; content is read only through the
overlay, and `verity=require` keeps the kernel enforcing fs-verity on every
backing object. Without fs-verity (`ocicfs version` prints
`verity: disabled`; an embedder asks `Engine.VerityRequired`), leave
`verity=require` out: the objects carry no fs-verity. A raw EROFS mount has no kernel digest option, so ocicfs
enforces the expected digest itself: it measures the image on the very file
descriptor it attaches to the loop device, making the mounted metadata the
measured metadata. This flat overlay is the supported writable pattern: stacking a
writable overlay directly on a composefs mount exceeds the kernel's filesystem
stacking depth. The upper and work directories must not live on an overlayfs.

## Notes

- **Locally resolved only.** Accepted forms are `name@sha256:<hex>`, a bare
  `sha256:<hex>`, and any ref string a previous pull recorded, such as a tag. A
  ref that pins no digest is resolved from the local reference record, never
  over the network, so a tag this store has not pulled is rejected.
- **Host platform only.** Mount looks for the binding matching the local OS and
  architecture, under the image identity for that platform. If that binding is
  absent, pull the image for this platform first.
- **Read-only only.** `--rw` and writable overlay options are rejected with
  `writable mounts are not supported`. A writable rootfs is assembled by the
  consumer over a `--metadata-only` mount instead (see above).
- **Fail closed.** With fs-verity (`verity: enabled`) ocicfs requires it for
  the composefs image and backing objects, and there is no unverified mount
  fallback: an image object without fs-verity fails the mount naming the
  remedy, and for a backing object without it the mount succeeds and opening
  that file fails with EIO. A build running without fs-verity
  (`verity: disabled`), decided by the build and the filesystem alone, is the
  only mount without it.
- **Mounts mark in use for gc.** A mount holds the store lock shared during
  setup and takes a shared flock on the image object — the in-use signal
  `ocicfs gc` probes before deleting. On both mount paths the lock rides the
  loop device, so it lives exactly as long as the mount and survives the
  ocicfs process; it still ends at unmount or reboot, so hold an image that
  must stay protected beyond that (see [gc.md](gc.md)). A mount that loses
  the unlink race with a concurrent collection is refused with the pull
  remedy instead of serving a deleted image.
- **Crash residue.** A hard kill in the narrow window between the scratch
  EROFS mount and its lazy detach leaves that scratch mount, its loop device,
  and the image's in-use lock behind until reboot. The residue lives on tmpfs
  (`/run/ocicfs`) and only over-retains store objects for one boot; it never
  loses data.
- **What the verity check binds.** The expected fs-verity digest — the sealing
  annotation of the digest-verified identity manifest for a sealed image, the
  binding's locally recorded digest otherwise — is enforced by measuring the
  image on the very descriptor the loop device attaches, on both mount paths;
  unless the build runs the store without fs-verity (`verity: disabled`),
  fs-verity is required on every backing object either way, so a backing object without fs-verity, or whose
  content differs from the digest the image metadata records, cannot be
  served. The binding itself is an
  untrusted, regenerable index: it selects which local image object is mounted
  for the requested identity digest, and for a sealed image it contributes
  nothing to the trust decision. For an unsealed image the enforced digest is
  the binding's own local record, not a publisher-provided value, so a
  coherent rewrite of binding and image object goes undetected; sealing closes
  that gap.
