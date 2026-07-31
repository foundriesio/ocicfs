# Mounting a pulled image (`knarr mount`)

`knarr mount` exposes an already-pulled image as a read-only composefs
filesystem. It does not pull, rebuild, or execute anything; it reads the local
binding and object store produced by `knarr pull`. The reference may pin a
digest (`name@sha256:<hex>` or a bare `sha256:<hex>`) or be any ref string a
previous pull recorded, such as a tag: an unpinned ref resolves locally —
never over the network — to the identity its most recent pull recorded, and a
pinned index digest resolves to the manifest that index offers for the host
platform. Every verification keys on that identity digest.

## Prerequisites

- A store on an **fs-verity-capable filesystem** containing the pulled image.
- `mount.composefs` on `PATH`.
- A target directory that already exists.
- Kernel support for composefs, overlayfs verity enforcement, and fs-verity.

### No fs-verity filesystem? Create one on a loop device

If the host filesystem does not support fs-verity, back the store with a
loopback ext4 that does (fs-verity needs a 4096-byte block size):

```sh
sudo fallocate -l 2G /var/lib/knarr.ext4
sudo mkfs.ext4 -F -b 4096 -O verity -m 0 /var/lib/knarr.ext4
sudo mkdir -p /var/lib/knarr
sudo mount -o loop /var/lib/knarr.ext4 /var/lib/knarr
```

## 1. Pull a digest-pinned image

See [pull.md](pull.md) for creating a local OCI layout and pulling it into the
store:

```sh
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
knarr pull --from-dir ./layout "app@$digest"
```

## 2. Create the mount target

knarr does not create or clean the target directory:

```sh
sudo mkdir -p /mnt/app
```

## 3. Mount read-only

```sh
sudo knarr mount "app@$digest" /mnt/app
```

Before anything else, mount resolves and digest-verifies the stored metadata
chain in `objects/sha256` in a single pass — the index when the reference pins
one, the platform manifest, and its config. Only then does it read the
host-platform binding from `images/` for the verified identity and pick the
fs-verity digest to enforce: the manifest's merged-image sealing annotation
when one is present, the digest the binding recorded at pull otherwise. It
re-measures the referenced composefs image object against that value and
passes it to `mount.composefs`. The mount fails if the metadata chain or the
image object is missing, unmeasurable, or measured differently from the
enforced digest.

The mounted tree is read-only:

```sh
find /mnt/app -maxdepth 2 -type f
cat /mnt/app/etc/config.txt
```

## 4. Unmount

```sh
sudo knarr unmount /mnt/app
```

`unmount` tears down the mount at the target path. It does not remove the target
directory or delete store content.

## Metadata-only mounts (`--metadata-only`)

A consumer that assembles its own writable overlay (a containerd snapshotter,
or a script-driven agent) mounts only the image's verified EROFS metadata and
folds the store's object directory into that overlay as a data-only lower:

```sh
sudo knarr mount --metadata-only "app@$digest" /mnt/app-meta
sudo mount -t overlay overlay -o \
  "lowerdir=/mnt/app-meta::/var/lib/knarr/objects/fsverity-sha256-12,upperdir=$upper,workdir=$work,redirect_dir=on,metacopy=on,verity=require" \
  /mnt/app
```

The metadata mount alone serves no file data; content is read only through the
overlay, and `verity=require` keeps the kernel enforcing fs-verity on every
backing object. A raw EROFS mount has no kernel digest option, so knarr
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
- **Fail closed.** knarr requires fs-verity for the composefs image and backing
  objects. There is no unverified mount fallback.
- **Mounts mark in use for gc.** A mount holds the store lock shared during
  setup and takes a shared flock on the image object — the in-use signal
  `knarr gc` probes before deleting. A metadata-only mount's lock rides the
  loop device and lives exactly as long as the mount; a composefs mount's
  lock ends with the knarr process, so hold an image you keep mounted
  long-term (see [gc.md](gc.md)). A mount that loses the unlink race with a
  concurrent collection is refused with the pull remedy instead of serving a
  deleted image.
- **What the verity check binds.** The expected fs-verity digest — the sealing
  annotation of the digest-verified identity manifest for a sealed image, the
  binding's locally recorded digest otherwise — is enforced on the composefs
  image by the kernel on a composefs mount, and by measuring the image on the
  loop-attached descriptor on a metadata-only mount; fs-verity is required on
  every backing object either way, so tampered or unsealed content cannot be
  served. The binding itself is an untrusted,
  regenerable index: it selects which local image object is mounted for the
  requested identity digest, and for a sealed image it contributes nothing to
  the trust decision.
