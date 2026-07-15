# Mounting a pulled image (`knarr mount`)

`knarr mount` exposes an already-pulled image as a read-only composefs
filesystem. It does not pull, rebuild, or execute anything; it reads the local
binding and object store produced by `knarr pull`. The reference may pin a
digest (`name@sha256:<hex>` or a bare `sha256:<hex>`) or be any ref string a
previous pull recorded, such as a tag: an unpinned ref resolves locally —
never over the network — to the digest its most recent pull recorded, and
every verification keys on that digest.

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

Mount reads the host-platform binding from `images/`, re-measures the referenced
composefs image object in `objects/sha256`, and passes the recorded fs-verity
digest to `mount.composefs`. The mount fails if the image object is missing,
unmeasurable, or measured differently from the binding.

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
  "lowerdir=/mnt/app-meta::/var/lib/knarr/objects/sha256,upperdir=$upper,workdir=$work,redirect_dir=on,metacopy=on,verity=require" \
  /mnt/app
```

The metadata mount alone serves no file data; content is read only through the
overlay, and `verity=require` keeps the kernel enforcing fs-verity on every
backing object. This flat overlay is the supported writable pattern: stacking a
writable overlay directly on a composefs mount exceeds the kernel's filesystem
stacking depth. The upper and work directories must not live on an overlayfs.

## Notes

- **Digest-pinned only.** Accepted forms are `name@sha256:<hex>` or a bare
  `sha256:<hex>`. A tag-only reference is rejected.
- **Host platform only.** Mount looks for the binding matching the local OS and
  architecture. If that binding is absent, pull the image for this platform first.
- **Read-only only.** `--rw` and writable overlay options are rejected with
  `writable mounts are not supported`. A writable rootfs is assembled by the
  consumer over a `--metadata-only` mount instead (see above).
- **Fail closed.** knarr requires fs-verity for the composefs image and backing
  objects. There is no unverified mount fallback.
- **What the verity check binds.** The kernel enforces the binding's recorded
  fs-verity digest on the composefs image and requires fs-verity on every
  backing object, so tampered or unsealed content cannot be served. The binding
  itself is an untrusted, regenerable index: it selects which locally verified
  image is mounted for the requested root digest, and is not itself sealed.
