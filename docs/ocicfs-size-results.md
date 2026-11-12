# ocicfs-size results: update estimates for arm64 images

Estimates from `ocicfs-size update` (run 2026-11-13) for updates of public
linux/arm64 images. On 2026-11-13 every update here was also pulled into
real ext4 stores on an amd64 VM, with containerd's overlayfs and EROFS
snapshotters and with ocicfs: each disk figure was within 1% of the
estimate and each download matched to the byte. The modeled delta and
inline rows were not checked. See
[how the sizes are calculated](ocicfs-size.md#how-the-sizes-are-calculated)
and [scope and accuracy](ocicfs-size.md#scope-and-accuracy).

## Key results

- **Free space for an update:** the ocicfs file CAS needs 2–6 times less
  than overlayfs or EROFS, because unchanged files are shared with the old
  images.
- **Disk after the update:** EROFS is up to 1% smaller than overlayfs. The
  file CAS is 7–21% smaller without fs-verity; with fs-verity it ranges
  from 14% smaller to 12% larger.
- **Download:** for images as published, nothing fetches less than
  containerd. Published as zstd:chunked, `ocicfs pull --delta` fetches
  48–94% less, except when most layers are unchanged.
- Tiny images gain little or nothing.
- **Inline threshold:** storing files of up to 4 KiB inline instead of
  64 bytes would cut file CAS disk with fs-verity by 3–11%, and 64 KiB by
  5–21%, which brings it below EROFS; in exchange the delta download
  grows by 23–80% at 4 KiB and 30–120% at 64 KiB, and free space for an
  update by up to 74%.

## Disk (MiB): free space needed / used after

Needed is the extra space while old and new images are both held; after is
with the old images removed. Overlayfs has no fs-verity; EROFS is the
containerd EROFS snapshotter with fs-verity.

| Update | Overlayfs | EROFS | File CAS, fs-verity | File CAS, no fs-verity |
|---|---|---|---|---|
| Arduino camera + sensor apps | 1,156 / 2,218 | 1,151 / 2,196 | 552 / 2,386 | 473 / 2,072 |
| Home Assistant, Frigate, node-red, mosquitto | 5,355 / 5,819 | 5,317 / 5,781 | 1,069 / 6,502 | 861 / 5,380 |
| nginx, redis, postgres | 833 / 833 | 831 / 831 | 185 / 714 | 173 / 661 |
| mosquitto | 16 / 16 | 15 / 15 | 14 / 19 | 13 / 17 |

## Download (MiB)

Images as published. None is zstd:chunked, so the last column is modeled.

| Update | containerd pull | ocicfs pull | ocicfs pull --delta, if zstd:chunked |
|---|---|---|---|
| Arduino camera + sensor apps | 378 | 446 | ~196 |
| Home Assistant, Frigate, node-red, mosquitto | 1,516 | 1,516 | ~510 |
| nginx, redis, postgres | 265 | 265 | ~74 |
| mosquitto | 8 | 8 | ~8 |

On Arduino, `ocicfs pull` reads one shared 68 MiB layer again.

Images republished as zstd:chunked:

| Update | containerd pull | ocicfs pull --delta |
|---|---|---|
| nginx 1.29.7 → 1.29.8 | 60 | 13 |
| node-red 4.1.11 → 4.1.12 | 166 | 19 |
| Edge Impulse runner v1.95.13 → v1.95.14 | 267 | 17 |
| Arduino Python dev image 95.2 → 95.3 | 133 | 20 |
| redis 8.10.1 → 8.10.2 (most layers unchanged) | 23 | 24 |

The model agrees with these: nginx ~13 vs 13, node-red ~20 vs 19.

## Inline threshold (MiB, file CAS with fs-verity)

ocicfs stores files of up to 64 bytes, the composefs default, inside the
image's metadata; every larger file is a separate object with its own
fs-verity Merkle tree. A larger threshold, which ocicfs does not support
today, saves those per-file costs, but inline files are not shared between
images, so the delta pull fetches them again on every update. composefs
tools build inline files of up to about 5,000 bytes, so the 64 KiB rows
are a projection.

| Update | Inline max | Needed / after | ocicfs pull --delta, if zstd:chunked |
|---|---|---|---|
| Arduino camera + sensor apps | 64 B | 552 / 2,386 | ~196 |
| | 2 KiB | 527 / 2,149 | ~250 |
| | 4 KiB | 550 / 2,115 | ~259 |
| | 64 KiB | 688 / 2,017 | ~305 |
| Home Assistant, Frigate, node-red, mosquitto | 64 B | 1,069 / 6,502 | ~510 |
| | 2 KiB | 1,086 / 5,962 | ~908 |
| | 4 KiB | 1,188 / 5,824 | ~919 |
| | 64 KiB | 1,862 / 5,134 | ~1,121 |
| nginx, redis, postgres | 64 B | 185 / 714 | ~74 |
| | 2 KiB | 187 / 696 | ~89 |
| | 4 KiB | 193 / 694 | ~91 |
| | 64 KiB | 219 / 680 | ~96 |

## Limits

- Disk excludes containerd's content store, writable layers, app data and
  garbage-collection delay.
- The modeled column was 4.5–6.5% below measured downloads; delta assumes
  the registry serves byte ranges.
- The disk figures are for `ocicfs pull`. `ocicfs pull --delta` saves a
  scan record only for a layer it reads whole, so it needed up to 1.8 MiB
  less than estimated in these updates.

Reproduce with `scripts/size-results.sh OUTDIR`; all references are pinned
by digest.
