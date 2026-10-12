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
├── layers/sha256/<aa>/<rest>    # saved layers: file lists by layer blob digest — auxiliary
├── refs/.lock                   # record-write lock (flock), never removed
├── refs/sha256/<kk>/<rest>.json # reference records — auxiliary
├── holds/sha256/<aa>/<rest>/<holder>   # GC-protection holds — auxiliary
├── .lock                        # store-level lock coordinating gc with pull/hold/mount
└── tmp/                         # staging + build scratch
```

`.lock` is created on demand and never removed: `pull`, `hold`, and `mount`
hold it shared for their duration, `gc` shared while it inventories the
objects and exclusive while it decides and deletes (failing fast when busy),
so a deletion never interleaves with an image being staged, a hold being
recorded, or a mount being set up. See [gc.md](gc.md).

Every digest is sharded by its first byte (`<aa>` = first two hex chars, `<rest>`
= the remaining 62) to keep directories shallow.

## `objects/` — the content-addressed store (authoritative)

A single decompressed copy of every regular file larger than 64 bytes (smaller
files live inline in their EROFS), keyed by its sha256 digest and sealed with
fs-verity. One pull writes three kinds of object here:

- **Metadata blobs** — the verified OCI chain: the root (index or manifest),
  the image manifest, and the config, each stored under its own digest.
- **Per-file content** — one object per regular file **larger than 64 bytes**,
  across all layers, so identical large files across images and layers are stored
  once. Files of 64 bytes or fewer are inlined into each composefs image's EROFS
  metadata (as `mkcomposefs` does) and keep no object here — a payload-bounded
  duplication accepted for upstream canonicality.
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
decompressed per-file content, and beside it the layer's file list (see
[`layers/`](#layers--saved-layers-auxiliary)). A `pull --delta` changes what crosses the wire,
not what lands here: the objects it writes are a subset of a plain pull's,
under the same names (a layer built from its table stores only the files
the image needs), and a whole layer it had to spool lives in `tmp/` only for
the pull's duration.

## Image identity — the digest everything is keyed by

An image is keyed by its **identity digest**: the digest of the platform image
manifest. A reference that lands on an image index — a tag usually does — is
descended one level, inside the index's own digest-verified bytes, to the
manifest it offers for the host platform, and that manifest's digest is what
`images/`, `holds/`, and every verb use. Pulling one image by its index digest
and by its platform manifest digest therefore stores it once, under one key,
with one set of holds. The digest `knarr pull`, `knarr ls`, and `knarr inspect`
report is the identity, which may differ from the digest the reference pinned;
it is the digest to pin, hold, and look the image up by. See
[ADR 0002](adr/0002-image-identity.md).

## `images/` — image bindings (auxiliary)

One JSON file per (identity digest, platform). It's a convenience index that
lets a mount, once the stored metadata chain has verified, go straight to the
already-built composefs image without rebuilding it, and records the locally
measured fs-verity digest:

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

At mount time, knarr first resolves and digest-verifies the stored metadata
chain from `objects/sha256` in a single pass — the index when the reference
pins one, the platform manifest, and its config — then reads the host-platform
binding for the verified identity and selects the fs-verity digest to enforce:
the manifest's merged-image sealing annotation when one is present,
`rootfs.fsverity.digest` otherwise. It then
opens the referenced composefs image, re-measures its fs-verity digest, and
requires it to match the selected value before mounting. The sealed value is
re-derived from the verified manifest on every mount instead of being recorded
in the binding (say, as another `fsverity.source`): the binding is untrusted
at mount, so a recorded provenance marker would be exactly as tamperable as
the digest it qualifies. The mount uses the binding's `objectStore.path`
(`objects/fsverity-sha256-12`, the fs-verity view) as the composefs basedir, so
the image's fs-verity-keyed redirects resolve to the sealed objects written by
pull.

## `layers/` — saved layers (auxiliary)

One file per layer blob a pull has read, named by the digest of the
**compressed** blob — the digest a manifest lists for the layer — and holding
the layer's file list as the scan of its tar stream produced it. A later pull
whose manifest names the same blob merges the layer from this file instead of
reading the blob (see [pull.md](pull.md#layers-already-read)). No file
content lives here but the bytes of files of 64 bytes or fewer, which the
image inlines; a larger file is named by its digest and stays an object in
`objects/`, which a saved layer does not keep alive.

The file is one zstd frame with a content checksum, holding one JSON value per
line: a header, then one record per tar entry, in tar order.

```json
{"version":1,"blobSize":2811,"diffID":"sha256:…"}
{"kind":"node","name":"dXNy","mode":2147484141,"mtime":1700000000}
{"kind":"node","name":"dXNyL2Jpbi9hcHA=","mode":493,"digest":"sha256:…","size":8192,"mtime":1700000000}
{"kind":"node","name":"ZXRjL2hvc3RuYW1l","mode":420,"content":"a25hcnIK","size":6,"mtime":1700000000}
{"kind":"hardlink","name":"dXNyL2Jpbi9hbGlhcw==","target":"dXNyL2Jpbi9hcHA="}
{"kind":"whiteout","name":"ZXRjLy53aC5vbGQ="}
{"kind":"opaque","name":"dmFyLy53aC4ud2guLm9wcQ=="}
```

`version` is the format version. `blobSize` and `diffID` are the size of the
compressed blob and the `diff_id` the layer was verified against when it was
saved; a pull uses the file only for a manifest and a config that name the
same two values for the blob. A record's `kind` is `node` (a file, directory,
symlink, device or fifo), `hardlink`, `whiteout` or `opaque`. `name`,
`target` (what a hardlink links to), `linkname` (a symlink's target), and
the `key` and `value` of each entry of `xattrs` are base64, since none of
them need be valid UTF-8. A whiteout or opaque marker is its name alone,
which says what it removes. A node carries
`mode` (type and permission bits, in Go's `os.FileMode` layout), `uid`,
`gid`, `mtime` and `mtimeNsec`, and for a device `devmajor` and `devminor`. A
regular file carries its `size` and either the `digest` of the object that
holds its content or, when it is small enough to be inlined, its bytes as
base64 `content`. A field holding its zero value is left out. No fs-verity
digest is recorded: a pull resolves each file it needs against the object
store.

The file is written atomically and durably, and is owner-only (0600) like
the objects, since it carries the bytes of the layer's smallest files. It is
at most 64 MiB, compressed and decompressed alike, and holds at most 262144
records; a layer whose file list would exceed either is not saved. A file
over those bounds, one that does not decompress to its end, is of another
version, holds a record a scan could not have produced, or records another
blob size or `diff_id` than the image names is ignored, and the layer is read
again. `knarr gc` deletes the saved layers no wanted image's manifest lists
(see [gc.md](gc.md)).

## `refs/` — reference records (auxiliary)

One JSON file per reference, keyed by `sha256(ref)` where `ref` is the engine's
normalized form of whatever string the caller passed to `pull` (a bare digest
passes through unchanged; anything else is canonicalized through the OCI
reference grammar, e.g. `ubuntu` becomes `index.docker.io/library/ubuntu:latest`).
Two spellings are fixed on top of that grammar: Docker Hub is spelled
`index.docker.io` (so `docker.io/foo` and `foo` both key as
`index.docker.io/library/foo:latest`), and a digest-pinned reference is keyed
as `name@digest` with any tag dropped (`host/app:v1@sha256:…` keys as
`host/app@sha256:…`). A name must satisfy the OCI distribution reference
grammar; a malformed one is rejected rather than recorded.
This lets a short or differently-formed alias and its fully qualified
equivalent converge on the same record. It maps a human ref to what it
resolved to, for `ls`, provenance, and tag→digest naming — it is **not** a
trust anchor:

```json
{
  "schema": "application/vnd.knarr.ref.v2+json",
  "ref": "app:v1",
  "refKey": { "algorithm": "sha256", "digest": "sha256:…" },
  "resolved": { "mediaType": "", "digest": "sha256:…", "size": 6694 },
  "identity": { "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": "sha256:…", "size": 481 },
  "pulledAt": "2026-07-01T11:47:00Z",
  "platforms": ["linux/arm64"],
  "holder": "cfs-build-…"
}
```

`holder` is present only on a *runtime reference*, written by a pull made
under `WithHold`: the record names the image for that holder and pins
nothing — `knarr gc` keeps the image through the hold, not the name, and
deletes the record once the holder is released. A record without `holder` is
an agent's, and keeps its image on its own.

The two descriptors say different things. `resolved` is **provenance**: the
document the reference itself landed on — an image index for the tag above, and
the manifest itself for a manifest-pinned reference, in which case the two
descriptors are the same one. It is unauthenticated local bookkeeping; no trust
decision takes it as an input, but `knarr gc` resolves it as a wanted root to
keep the pulled document reachable, so a record whose resolved chain is broken
aborts a collection. `identity` is the platform image manifest the reference
names, the digest the bindings and holds are keyed by and the one every verb
looks the image up under. A descriptor records a `mediaType` only when the
document that named it declared one, so `resolved` carries an empty one (a
pinned root has no parent descriptor), as does `identity` when the reference
already named the manifest. A record carrying an older schema is rejected on read,
naming the remedy (recreate the store); `ls` skips such a record with a warning
rather than failing the whole listing.

`knarr ls` reads these records and reports one row per stored reference, showing
the identity digest. `knarr remove <ref>` deletes the matching ref record and
any unshared image binding for the recorded identity digest and platform; it
does not delete content from `objects/` — `knarr gc` reclaims objects nothing
references (see [gc.md](gc.md)).

## `holds/` — GC-protection holds (auxiliary)

A hold pins an image's content so a `remove` declines while any holder is
present. Each hold is an empty marker file named for its holder under the
digest's holder directory:

```
holds/sha256/<aa>/<rest>/<holder>
```

Holds are keyed by the **identity digest**, not the reference: holds protect
content identity, and the reference records are auxiliary (a tag can be
repointed, and a holder may pin an image by bare digest that no local ref
names). Both verbs resolve their argument down to that one digest first, so a
hold taken on an image under one name is seen by a `remove` of any other name
for it. A holder name is
restricted to `[a-z0-9._-]` so it can only ever name a single file inside the
directory. The holder directory is removed with its last holder; the content in
`objects/` is never touched.

The two verbs differ in what they make of a store they cannot read. `hold`
fails on exactly one condition — a stored blob that does not hash to the digest
naming it, which is corruption a hold must not paper over. Any other resolution
failure holds the digest as given: content absent from the store is reported as
not pulled, and a plain content object (anything that does not parse as OCI
metadata) is held as the object it is. `release` never fails on store state at
all: whatever shape the store is in, the holder is dropped, so a hold can always
be undone.

`remove <ref>` reads the holders for the record's identity digest and declines
while any are present, naming each holder **and the digest they sit under** —
that digest is what a `release` takes, and it is not always the one typed at the
prompt. `remove --force <ref>` clears the holds first and proceeds.

Mutating a digest's holders — and the `remove` check-then-delete, which must be
atomic against a concurrent hold — runs under a per-digest advisory lock
(`flock`) on a dedicated lock file:

```
holds/.locks/sha256/<aa>/<rest>
```

The lock file is created on demand and **never removed**, so a last-holder
cleanup that removes the holder directory cannot invalidate a lock a concurrent
caller is waiting on. The lock files are empty and carry no content.

## Authoritative vs. auxiliary

`images/` and `refs/` are **regenerable indexes**, not trust anchors: all content
lives in `objects/`, and re-pulling a pinned digest rebuilds both indexes without
re-storing it. Because they are not trusted, bindings are verified on use against
the object store — a binding's composefs image is content-addressed there, and at
mount its measured fs-verity digest must match the enforced value (the identity
manifest's sealing annotation for a sealed image, the binding's recorded digest
otherwise), so a stale or tampered binding cannot serve unverified content. Ref
records are validated when read (schema, ref key, and sha256 digests), but they
are not content trust anchors.

`layers/` is auxiliary too: losing a saved layer costs the next pull a read of
that layer and nothing else. Unlike a binding, a saved layer is not checked
against the object store when it is used — only its own checksum and the blob
size and `diff_id` it records are — so what it contributes to an image is
verified only where the image is sealed: the merged image must then measure
to the sealing annotation at pull and at mount. See
[pull.md](pull.md#layers-already-read).
