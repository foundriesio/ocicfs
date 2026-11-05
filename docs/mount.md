# Mounting a pulled image (`ocicfs mount`)

Use `ocicfs mount` to expose a pulled image as a read-only composefs
filesystem. It reads the local store prepared by `ocicfs pull`. It does
not pull, rebuild, or execute anything.

## Prerequisites

- A store containing the pulled image. fs-verity is optional and adds
  checks on every read. The default build works without it; see
  [the fs-verity mode](on-disk-layout.md#the-fs-verity-mode).
- Kernel support for EROFS and the required overlayfs features, plus
  fs-verity for a store with fs-verity: Linux 6.6, or a vendor kernel with
  the same patches. See [Kernel requirements](#kernel-requirements).

### Optional: add fs-verity with a loop device

To add fs-verity on a host whose filesystem cannot enable it, put the
store on an ext4 filesystem backed by a loop device. The default build
runs without fs-verity on such a host; a `required` build refuses the
store. fs-verity needs a 4096-byte block size:

```sh
sudo fallocate -l 2G /var/lib/ocicfs.ext4
sudo mkfs.ext4 -F -b 4096 -O verity -m 0 /var/lib/ocicfs.ext4
sudo mkdir -p /var/lib/ocicfs
sudo mount -o loop /var/lib/ocicfs.ext4 /var/lib/ocicfs
```

## 1. Pull a digest-pinned image

See [pull.md](pull.md) for creating a local OCI layout. Pull its image
into the store:

```sh
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
sudo ocicfs pull --from-dir ./layout "app@$digest"
```

## 2. Create the mount target

```sh
sudo mkdir -p /mnt/app
```

ocicfs does not create or clean the target directory. The directory must
already exist. ocicfs never stacks mounts: if the target is already a
mountpoint, it refuses to mount over it. Unmount it first.

## 3. Mount read-only

```sh
sudo ocicfs mount "app@$digest" /mnt/app
```

The image's files are now available at `/mnt/app`. The mounted tree is
read-only:

```sh
find /mnt/app -maxdepth 2 -type f
cat /mnt/app/etc/config.txt
```

## 4. Unmount

```sh
sudo ocicfs unmount /mnt/app
```

`unmount` tears down the mount at the target path. It does not remove the
target directory or delete store content.

## Metadata-only mounts (`--metadata-only`)

To build a writable filesystem, mount the image's verified EROFS metadata
(see [Verification and trust](#verification-and-trust)) and add your own
overlay. A containerd snapshotter or a script-driven agent can use this
pattern.

Create the metadata target and the writable directories. The upper and
work directories must be on the same filesystem, not on overlayfs, and
the work directory must be empty.

This example is for `verity: enabled`. With `verity: disabled`, leave out
`,verity=require` (check with `ocicfs version`; an embedder checks
`Engine.VerityRequired`).

```sh
upper=/var/lib/app/upper   # any non-overlayfs path
work=/var/lib/app/work
sudo mkdir -p /mnt/app-meta /mnt/app "$upper" "$work"
sudo ocicfs mount --metadata-only "app@$digest" /mnt/app-meta
sudo mount -t overlay overlay -o \
  "lowerdir=/mnt/app-meta::/var/lib/ocicfs/objects/fsverity-sha256-12,upperdir=$upper,workdir=$work,redirect_dir=on,metacopy=on,verity=require" \
  /mnt/app
```

The metadata mount alone serves no file data. The overlay combines that
metadata with the store's object directory as a data-only lower layer.
Content is read through the overlay. With `verity: enabled`,
`verity=require` makes the kernel enforce fs-verity on every backing
object. With `verity: disabled` the option is left out, as above: this
mode does not require fs-verity on backing objects.

This flat overlay is the supported writable pattern. Stacking a writable
overlay directly on a composefs mount exceeds the kernel's filesystem
stacking depth. The upper and work directories must not live on an
overlayfs.

`ocicfs mount --rw` and writable overlay options are rejected with
`writable mounts are not supported`.

## Notes

### References and platform

Mount accepts `name@sha256:<hex>`, a bare `sha256:<hex>`, or any reference
string recorded by a previous pull, such as a tag.

A reference without a digest resolves to the identity recorded by its
most recent pull. Resolution is local only; mount never contacts the
network. A tag that the store has not pulled is rejected.

A pinned index digest resolves to the manifest it offers for the host
platform. That manifest's digest is the image identity used for every
verification.

Mount uses only the local OS and architecture. It needs the matching
local image record, called a binding, under that platform's image
identity. If the binding is absent, pull the image for this platform
first.

### Verification and trust

Both mount modes verify the stored metadata chain before reading the
binding. In a single pass, mount checks the digests of the metadata in
`objects/sha256`: the index when the reference pins one, the platform
manifest, and its config.

Mount then reads the host-platform binding from `images/` for the verified
identity. The binding selects the local image object to mount. Mount
chooses the expected EROFS fs-verity digest as follows:

- For a sealed image, use the merged-image sealing annotation from the
  verified manifest.
- Otherwise, use the digest the binding recorded at pull.

A raw EROFS mount has no kernel digest option, so ocicfs enforces this
check itself. On both mount paths, it re-measures the image on the same
file descriptor it attaches to a read-only loop device. With fs-verity,
the image cannot change after it is measured. Without it (`verity: disabled`),
the digest is computed from that descriptor's bytes, but a later change
to the image file is not detected.

Mount fails if the metadata chain is missing or fails verification. It
also fails if the image object is missing, cannot be measured, or has a
different fs-verity digest from the expected value.

With fs-verity (`verity: enabled`), fs-verity is required for the image
and every backing object. An image object without fs-verity fails the
mount and names the remedy. Through the overlay, the kernel refuses to
serve a backing object without fs-verity or with content that differs
from the digest in the EROFS metadata: the mount succeeds and opening
that file fails with EIO. For metadata-only mounts, the consumer supplies
that overlay as shown [above](#metadata-only-mounts---metadata-only).
There is no unverified mount fallback. The only mount without fs-verity
is a build running without it (`verity: disabled`), which the build and
the filesystem decide alone.

The binding is an untrusted index that can be regenerated. For a sealed
image, it contributes nothing to the trust decision.

For an unsealed image, the expected digest is the binding's local record,
not a publisher-provided value. A coherent rewrite of both the binding
and the image object goes undetected. Sealing closes this gap.

### How the read-only mount is assembled

After verification, ocicfs mounts the image's EROFS metadata on a private
scratch directory under `/run/ocicfs`. It then creates one read-only
overlay at the target. The overlay uses the store's object directory as
a data-only lower layer and sets `verity=require`; without fs-verity
(`verity: disabled`) it has no `verity=` option.

Before returning, ocicfs lazily detaches the scratch mountpoint. The
overlay keeps the metadata, loop device, and image's in-use lock alive
until the target is unmounted.

### Mounts and garbage collection

During setup, mount holds the store lock shared. It also takes a shared
flock on the image object. This lock marks the image as in use;
`ocicfs gc` checks it before deleting.

On both mount paths, the loop device retains the lock. It survives the
ocicfs process and lasts as long as the mount. The protection ends at
unmount or reboot. Hold an image that must stay protected beyond that;
see [gc.md](gc.md).

If a concurrent collection unlinks the image before mount secures it,
mount refuses to serve the deleted image and tells the caller to pull
again.

### Crash residue

A hard kill between the scratch EROFS mount and its lazy detach leaves
the scratch mount, loop device, and image's in-use lock behind until
reboot.

This residue lives on tmpfs under `/run/ocicfs`. It only retains store
objects longer than needed, for one boot. It never loses data.

### Kernel requirements

The kernel needs EROFS and these overlayfs features:

- Data-only lower layers: mainline commit `37ebf056d6cf`, Linux 6.5.
- For a store with fs-verity, fs-verity and its validation of lower data
  for `verity=require`: the Linux 6.6 series through mainline commit
  `184996e92e86`, "ovl: Validate verity xattr when resolving lowerdata".
  This is the last patch the read-only path requires.

Use Linux 6.6 or a vendor kernel carrying all of these features. Below
that floor, mount fails with the overlay mount's error. There is no
fallback.
