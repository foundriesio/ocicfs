# On-disk store layout

This describes what knarr writes under its data root (`--root`, default
`/var/lib/knarr`) — the tree a `pull` produces and a `mount` reads. The store
filesystem must support **fs-verity** (e.g. ext4 formatted with `-b 4096`); knarr
seals every content object with it and refuses a store that cannot.

```
<root>/
├── objects/sha256/<aa>/<rest>   # the content-addressed object store (CAS) — authoritative
├── objects/fsverity-sha256-12/<vv>/<rest>   # fs-verity view into the CAS — auxiliary
├── images/sha256/<aa>/<rest>/<os>-<arch>[-<variant>].json   # image bindings — auxiliary
├── refs/sha256/<kk>/<rest>.json # reference records — auxiliary
└── tmp/                         # staging + build scratch
```

Every digest is sharded by its first byte (`<aa>` = first two hex chars, `<rest>`
= the remaining 62) to keep directories shallow.

## `objects/` — the content-addressed store (authoritative)

A single decompressed copy of every piece of content, keyed by its sha256 digest
and sealed with fs-verity. One pull writes three kinds of object here:

- **Metadata blobs** — the verified OCI chain: the root (index or manifest),
  the image manifest, and the config, each stored under its own digest.
- **Per-file content** — one object per regular file across all layers, so
  identical files across images and layers are stored once.
- **The composefs image** — the EROFS image that `mount -t composefs` mounts,
  whose per-file redirects reference the objects by their fs-verity digest
  through the view below.

Alongside the sha256 tree, `objects/fsverity-sha256-12/` holds a second name for
every object committed under this scheme: a hardlink sharded and keyed by the
object's measured fs-verity digest (`<vv>` = first two hex chars of that digest,
`<rest>` the rest; no `sha256:` prefix in the path). It shares one inode with the
sha256 name, so it adds no content and carries the same fs-verity seal. The view
is **auxiliary and regenerable** — rebuildable by walking `objects/sha256` and
re-measuring — never a trust anchor of its own. Objects written by earlier knarr
versions are not backfilled; each gains its view name when a write next
deduplicates onto it.

The object store is the **source of truth**: every byte a mount serves is
content-addressed and fs-verity-sealed here, and the auxiliary indexes below are
never trusted, only verified against it. The indexes are still the only local
map from a pinned root to its composefs image — losing them loses no content,
and a re-pull regenerates them while deduplicating against the objects already
present. The compressed wire form of a layer is **never** persisted — only its
decompressed per-file content.

## `images/` — image bindings (auxiliary)

One JSON file per (pinned root digest, platform). It's a convenience index that
lets a mount go straight to the already-built composefs image without re-resolving
or rebuilding, and records the locally measured fs-verity digest:

```json
{
  "schema": "application/vnd.knarr.cfs-binding.v1+json",
  "platform": { "os": "linux", "architecture": "arm64" },
  "rootfs": {
    "type": "composefs",
    "descriptor": { "mediaType": "application/vnd.knarr.composefs.image.v1+erofs",
                    "digest": "sha256:…", "size": 12288 },
    "objectStore": { "type": "composefs-digest-store", "algorithm": "fsverity-sha256-12", "path": "objects/fsverity-sha256-12" },
    "fsverity": { "mode": "required", "digest": "sha256:…", "source": "local-measurement" },
    "canonical": false,
    "size": 123456789
  }
}
```

`rootfs.objectStore` names the view a mount resolves the image's redirects
through, and is the binding's **compatibility key**: a binding carrying any other
tuple is treated as a cache miss and rebuilt. `rootfs.canonical` records whether
the merged image was built by the upstream-reproducible construction.

`rootfs.size` is display metadata recorded at build time — the merged tree's
regular-file content bytes, hardlink groups counted once. It is not part of the
trust chain, and is absent in bindings written before it existed. A pull that
verifies the image as already present does not rewrite the binding, so the
field appears when the image is next actually built (after the binding's last
ref is removed, or on an image update).

At mount time, knarr reads the host-platform binding for the pinned root digest,
opens the referenced composefs image from `objects/sha256`, re-measures that
image object's fs-verity digest, and requires it to match `rootfs.fsverity.digest`
before mounting. The mount uses the binding's `objectStore.path`
(`objects/fsverity-sha256-12`, the fs-verity view) as the composefs basedir, so
the image's fs-verity-keyed redirects resolve to the sealed objects written by
pull.

## `refs/` — reference records (auxiliary)

One JSON file per reference, keyed by `sha256(ref)` where `ref` is the engine's
normalized form of whatever string the caller passed to `pull` (a bare digest
passes through unchanged; anything else is canonicalized through the OCI
reference grammar, e.g. `ubuntu` becomes `index.docker.io/library/ubuntu:latest`).
This lets a short or differently-formed alias and its fully qualified
equivalent converge on the same record. It maps a human ref to what it
resolved to, for `ls`, provenance, and tag→digest naming — it is **not** a
trust anchor:

```json
{
  "schema": "application/vnd.knarr.ref.v1+json",
  "ref": "app@sha256:…",
  "refKey": { "algorithm": "sha256", "digest": "sha256:…" },
  "resolved": { "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": "sha256:…", "size": 481 },
  "pulledAt": "2026-07-01T11:47:00Z",
  "platforms": ["linux/arm64"]
}
```

`knarr ls` reads these records and reports one row per stored reference. `knarr
remove <ref>` deletes the matching ref record and any unshared image binding for
the recorded root digest and platform; it does not delete content from
`objects/`.

## Authoritative vs. auxiliary

`images/` and `refs/` are **regenerable indexes**, not trust anchors: all content
lives in `objects/`, and re-pulling a pinned digest rebuilds both indexes without
re-storing it. Because they are not trusted, bindings are verified on use against
the object store — a binding's composefs image is content-addressed there and its
fs-verity digest is enforced at mount, so a stale or tampered binding cannot
serve unverified content. Ref records are validated when read (schema, ref key,
and sha256 digests), but they are not content trust anchors.
