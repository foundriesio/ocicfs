# Pulling images (`ocicfs pull`)

Use `ocicfs pull` to put an OCI image in the local store. It verifies the
image metadata and content, stores decompressed files, builds the composefs
image, and records the image and its reference.

The source can be a registry, or, for air-gapped or test setups, an OCI
layout directory (`--from-dir`) or an image archive (`--from-tar`). A pull
reads whole layers in gzip, zstd or uncompressed form and needs no special
layer format; [delta pulls](#delta-pull), below, are the exception.

Pull can skip [layers already read](#layers-already-read). For updates,
delta pulls can fetch only missing files from zstd:chunked layers.

## Prerequisites

- fs-verity is optional and adds checks on every read. The default build
  works without it. A `required` build fails before writing anything if
  the store's filesystem cannot enable it. See
  [the fs-verity mode](on-disk-layout.md#the-fs-verity-mode).
- Install `mkcomposefs` from the composefs tools and put it on `PATH`.
- Choose a registry reference or a local OCI layout or archive. Use a
  digest-pinned reference in production. The source-specific reference
  forms are described below.

## Registry Pull

Pull a digest-pinned reference:

```sh
digest=$(crane digest ghcr.io/example/app:1.2.3)
ocicfs pull "ghcr.io/example/app@$digest"
```

The pinned digest is the trust anchor. The agent driving ocicfs must choose
and verify it.

The store defaults to `/var/lib/ocicfs`. Use `--root <dir>` to choose
another location. On success, pull prints the reference and stored digest:

```
ghcr.io/example/app@sha256:<digest>	sha256:<digest>
```

Use `--format json` for machine-readable output:

```sh
ocicfs pull "ghcr.io/example/app@$digest" --format json
# {"ref":"ghcr.io/example/app@sha256:...","digest":"sha256:...","size":12288}
```

For testing, use a tag:

```sh
ocicfs pull ghcr.io/example/app:1.2.3
# ghcr.io/example/app:1.2.3	sha256:<resolved digest>
```

A tag is a mutable alias. Resolving it takes one extra registry
round-trip and trusts the digest the registry currently serves. After
resolution, pull verifies content just as it does for a pinned reference.
Use the [printed digest](#the-printed-digest-is-the-image-identity) to pin
the image in production.

The store records the tag as an alias and stores the image under its
digest. Later, `mount`, `inspect`, and `remove` accept that same reference.
They resolve an unpinned reference locally to the digest its most recent
pull recorded, without contacting the registry.

Registry pulls accept `name@sha256:<hex>` and `name:tag`. A name with
neither a digest nor a tag uses `:latest`. Bare digests are rejected because
they name no repository to fetch from.

Registry pulls use HTTPS. A loopback registry, addressed by localhost or
a loopback IP, may use plain HTTP. Content integrity comes from the digest.
For a tag, resolution also trusts the loopback channel. There is no
insecure-registry option for other hosts. Pull emits no progress output.

Use `--ingest-workers N` to set how many layers are scanned concurrently
(default 2). Each registry layer streams while it is scanned, so N also
limits concurrent blob downloads. The cap is shared by all builds of one
engine. A separate `ocicfs pull` process adds its own workers to any daemon
already running.

Store writes, and fs-verity sealing when it is enabled, limit scan
throughput. More workers help only on fast storage. Keep the default on
an SD card or eMMC.

## The Printed Digest Is the Image Identity

Use the digest printed by pull to pin, hold, or look up the image:

```sh
ocicfs pull ghcr.io/example/app:1.2.3
# ghcr.io/example/app:1.2.3	sha256:<the host platform's manifest>
```

A tag usually resolves to an image *index*, which lists a manifest for each
platform. ocicfs verifies the index and descends one level to the host
platform's manifest. That manifest's digest is the image's **identity**.
It may differ from the digest pinned in the reference.

Pulling by the index digest or the platform manifest digest stores the
image once, with one set of holds. The reference record keeps the resolved
index digest in its `resolved` field as provenance. See
[on-disk-layout.md](on-disk-layout.md).

A later `pull`, `mount`, `inspect`, `hold`, or `release` given the index
digest descends it again and reaches the same image.

## Local OCI Layout Sideload

Create a standard OCI image layout with a tool such as
[crane](https://github.com/google/go-containerregistry):

```sh
crane pull --format=oci ghcr.io/example/app:1.2.3 ./layout
```

This creates `./layout/` with `oci-layout`, `index.json`, and
`blobs/sha256/`. Pull a digest present in that layout:

```sh
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
ocicfs pull --from-dir ./layout "app@$digest"
```

`--from-dir` accepts `name@sha256:<hex>` or a bare `sha256:<hex>`.
The directory supplies the source, so a repository name is optional.
A tag is not accepted because there is no registry to resolve it.

The digest may name an index or a manifest. An index resolves to the host
platform's manifest, as described under
[image identity](#the-printed-digest-is-the-image-identity).

`--ingest-workers N` uses the same cap as a registry pull, but reads layers
from the directory. `--delta` also works with a layout; see
[Delta Pull](#delta-pull) for its requirements and fallbacks.

### From a `docker save` or `skopeo` archive

Save an image and pull directly from the archive:

```sh
docker save --platform linux/arm64 -o app.tar ghcr.io/example/app:1.2.3
ocicfs pull --from-tar app.tar
```

`--from-tar` reads, in place, the uncompressed OCI layout archive that
`docker save` and `skopeo copy … oci-archive:` write. Nothing is extracted.

Without a reference, ocicfs uses the name carried by the archive. It
replaces that name's tag or digest with the digest of the single
`index.json` entry. Here, it records
`ghcr.io/example/app@<manifest digest>`.

The name comes from the entry's `io.containerd.image.name` annotation,
where `docker save` keeps its input reference. Otherwise, ocicfs uses a
fully qualified reference in `org.opencontainers.image.ref.name`.
Skopeo writes that annotation from the reference after the archive path:

```sh
skopeo copy --preserve-digests docker://ghcr.io/example/app:1.2.3 \
  oci-archive:app.tar:ghcr.io/example/app:1.2.3
ocicfs pull --from-tar app.tar
```

A `ref.name` that is not fully qualified does not identify an image.
This includes the bare tag that `docker save` also writes there. If
neither annotation supplies a usable name, pass a reference.

The archive's name identifies its contents; it does not authorize them.
An update agent should pass the digest from its own trusted metadata:

```sh
ocicfs pull --from-tar app.tar "ghcr.io/example/app@$digest"
```

The pull fails unless the archive holds that digest. It also checks any
seal on the manifest against the image it builds. The pinned digest
authenticates the files that land.

A rewritten manifest cannot match the original pin. `docker save` from
a daemon using the legacy graphdriver image store reconstructs a manifest
whose digest matches nothing published. Use skopeo or a daemon using the
containerd image store. For the latter, `docker info` reports
`io.containerd.snapshotter.v1`.

You can pass `name@sha256:<hex>`, a bare `sha256:<hex>`, or a bare name:

```sh
ocicfs pull --from-tar app.tar ghcr.io/example/app
```

A bare name is pinned to the single manifest in the archive's `index.json`.
Its tag is not recorded; the reference is stored as `name@digest`, as with
`--from-dir`.

Keep these archive constraints in mind:

- `docker save --platform` exports the platform's child manifest but keeps
  its input reference in the name annotation. That annotation can name an
  index digest even when the archive holds only the child. An explicit
  pin must name a digest the archive actually holds.
- An archive with several images requires a pin. Otherwise, pull refuses
  it and lists the entries' digests and names.
- An archive written by `ocicfs copy` for one index image lists both the
  index and its platform manifest. This counts as one image. Without a
  pin, ocicfs records the index.
- Gzip-, zstd-, xz- and bzip2-compressed archives are refused. Decompress
  the archive first. This restriction concerns the archive, not its
  individual layers.
- `--delta` and `--ingest-workers` apply as they do with `--from-dir`.

### Writing an archive with `ocicfs copy`

Pack registry images into an archive on a build host or in CI:

```sh
ocicfs copy --platform linux/arm64 -o images.tar \
  ghcr.io/example/app:1.2.3 ghcr.io/example/db@sha256:<digest>
```

`ocicfs copy` needs no store, privileges, or Docker daemon. It writes one
archive that both `--from-tar` and `docker load` can read.

On the device, pull each image using the digest its compose file names.
The archive selects an image by that digest, not by its repository name.
Then load the archive once:

```sh
app_pin=sha256:...   # the digests from your trusted compose file
db_pin=sha256:...
sudo ocicfs pull --from-tar images.tar "ghcr.io/example/app@$app_pin"
sudo ocicfs pull --from-tar images.tar "ghcr.io/example/db@$db_pin"
docker load -i images.tar
```

The archive preserves the registry's bytes, including every digest and the
publisher's seal. It includes only the blobs for `--platform`, which
defaults to the host platform. Shared blobs are written once.

For an image rooted at an index, `index.json` has two entries:

- The index, named by the reference passed to copy: its tag, or its digest
  if the reference carries only a digest.
- The platform manifest, named by that manifest's digest.

A pin on either digest resolves for both `--from-tar` and the image Docker
loads. Both `io.containerd.image.name` and
`org.opencontainers.image.ref.name` carry the full name.

Copy verifies every blob as it writes it. The archive appears under its
output name only when complete. The command prints each `index.json`
entry as `<name>\t<digest>`.

## Layers Already Read

Pull successive releases normally:

```sh
ocicfs pull "ghcr.io/example/app@$previous"
ocicfs pull "ghcr.io/example/app@$next"   # reads only the layers $previous lacks
```

When unchanged layers have usable saved file lists and the store still
holds the files the image needs, pull does not read those layers again.
It downloads, decompresses, and hashes none of their blob content.

After reading a layer's tar stream to the end, pull verifies:

- The blob's digest and size against the manifest.
- The unpacked stream's digest against the config's `diff_id`.

It then saves the layer's file list under the blob digest. A later pull
can reuse it when the blob digest, blob size, and `diff_id` all match.
Recompressing the same content changes the blob digest, so that layer
must be read again.

The list records each stored file's SHA-256 digest and size. It also
contains the bytes of files small enough to be inlined into the image.
The other file contents remain store objects. See
[on-disk-layout.md](on-disk-layout.md).

### When a layer is read again

Pull merges all layers before looking up the files the final image keeps.
It does not need files overwritten or removed by later layers, even if
`gc` has reclaimed them.

If a needed file is absent, pull reads the layer that lists it whole,
once. This can happen when an earlier release hid the file and `gc`
reclaimed it, but the new release needs it again. Pull stores every file
in that layer and verifies its blob digest and `diff_id` again.

Files under `/run` are an exception. They are removed only after the
merge, so a missing file there can still cause a layer to be read.

Pull also reads a layer again when its saved list is absent or unusable.
It ignores a list with a warning if the list:

- Does not decompress to its end.
- Uses another format version or contains a malformed record.
- Exceeds the size bounds.
- Records another blob size or `diff_id`.

A list that is too large to save, or cannot be written, causes a warning
rather than a failed pull. `gc` deletes lists for layers that no wanted
image lists; see [gc.md](gc.md).

Reusing lists builds an image bit-identical to a pull into an empty store.
Both paths accept and reject the same image content. One difference is
intentional: if no layer needs reading, pull can succeed even when the
source no longer serves the layer blobs.

### Trust and recovery

Saved file lists and image bindings rely on the assumption that only
ocicfs writes to the store. A list's checksum detects corruption, not
deliberate modification. Pull does not recheck a reused list against the
`diff_id`.

**Only sealed images are protected against a modified store.** For a
sealed image, pull and mount check the merged image's fs-verity digest
against the publisher's annotation. See
[Sealing Verification](#sealing-verification). File contents read later
are checked only with fs-verity: without it (`verity: disabled`), a
backing file modified after the pull is served even for a sealed image.
For an unsealed image, an
altered list can silently affect that image and every later image sharing
the layer.

Pull removes the saved lists it used if they could have caused a failure:

- The layers cannot be merged.
- A listed file cannot be found in the layer that claims to contain it.
- The image cannot be built.
- Sealing verification fails.

A retry then reads those layers from the source. Cancellation or a source
that cannot serve a layer leaves the saved lists in place.

Layer reuse applies to registry pulls, local sideloads, and an embedder's
`NewBuild`. In a delta pull, it applies to layers without a usable table
of contents.

## Delta Pull

Hold the previous release, pull the next one with `--delta`, and release
the old one after switching the workload:

```sh
ocicfs hold --holder delta-base "ghcr.io/example/app@$previous"
ocicfs pull --delta "ghcr.io/example/app@$next"
# ... switch the workload over to $next ...
ocicfs release --holder delta-base "ghcr.io/example/app@$previous"
ocicfs remove "ghcr.io/example/app@$previous"
ocicfs gc
```

A delta pull uses byte ranges to fetch a layer's table of contents and the
files the store lacks. This needs **zstd:chunked** layers. Other layers,
and cases where ranges do not help, use the fallbacks below.

The hold keeps release N−1's files available while the agent pulls N.
Repointing a tag or removing an old reference cannot lose that base.
A plain `remove` refuses a held image, so release the hold first.

ocicfs has no keep-previous policy. Without a hold or another reason to
keep the image, `gc` can reclaim it. The same update sequence works with
`--from-dir`.

### What determines the saving

A plain pull reads each changed layer whole. Rebuilding an image can
change every layer digest even when few files changed.

Delta savings are bounded by the file bytes shared between releases.
Compression, range coalescing, and fallback thresholds determine how much
of that saving a pull achieves. The publisher controls the shared content:
regenerating compiled artifacts, gzip-compressed documentation, or
packages can change file digests without a meaningful content change.

Measured on official images: consecutive patch releases of the same lines
(nginx, debian, python) shared 69 % of their file bytes on average and
85 % in the median update. A release that also rebuilt its base image
shared 7 % (two `python` builds a month apart) or less than 1 %, so
`--delta` saved nothing there. Keep unchanged files byte-identical between
releases to benefit from delta pulls.

### Layer format and verification

Publish zstd:chunked layers with
`skopeo copy --dest-compress-format zstd:chunked`.

These are standard `tar+zstd` blobs with a per-file table of contents.
Their descriptors carry the
`io.github.containers.zstd-chunked.manifest-position` and
`manifest-checksum` annotations. Pull fetches the table as one range and
authenticates it with the checksum. It verifies each file against the
digest recorded in the table.

The metadata chain, sealing annotations, and composefs image receive the
same checks as a plain pull. Both paths build bit-identical images that
reference the same objects and record the same binding.

For a layer built from its table, pull does **not** verify the `diff_id`:
it never decompresses the whole tar stream to hash it. The value is still
reported by `inspect`, chain IDs, and JSON. A layer read through its tar
stream receives plain-pull verification. A reused saved list received
that verification when it was written.

A plain pull stores files that later layers overwrite or remove; `gc`
reclaims them. A layer built from its table stores only files the final
image needs. Falling back to the tar stream stores every file in that
layer. Reading a whole blob still transfers overwritten or removed files'
bytes even when pull does not store them.

### Fallbacks and logs

Read stderr to see how each layer was fetched:

```text
level=INFO msg="delta: layer" layer=sha256:… size=1452 toc_bytes=381 toc_ms=3 entries=5 missing=2 planned=369 requests=1 fetched=750 fallback=none
```

Each layer gets one line:

- `missing` counts files the store lacked.
- `planned` and `requests` describe the bytes and ranges planned for those
  files. If the table request returns the whole blob, no file ranges are
  requested and `requests=0`.
- `fetched` counts bytes actually read for the layer, including the table.
- `fallback` explains why pull did not use ranges.

The fallback values are:

- **`no-toc`:** The layer has no table, as with a plain `tar+gzip` layer.
  Pull streams and unpacks it as a plain pull would.
- **`toc-error`:** The table could not be read or verified. Pull logs a
  warning with the cause, then streams and unpacks the layer. It verifies
  the whole blob against the manifest, so a bad table can cost bandwidth
  but cannot compromise integrity.
- **`requests` or `fraction`:** Missing files would require more than 256
  ranges or more than half the blob. Pull reads the blob once, in one
  request, and unpacks only missing files. A rebuilt base image triggers
  this rule before the first file request.
- **`whole-answer`:** The registry answered a range request with the whole
  blob because it lacks `Range` support. Pull reads the rest of the layer
  from that answer.
- **`saved`:** A layer without a usable table is merged from its saved
  file list and reports `fetched=0`. If a needed file is missing and a
  table also lists it, pull fetches it by range. If only the saved list
  names it, pull reads the layer whole and reports the blob's size as
  `fetched`. See [Layers Already Read](#layers-already-read).

Layers scanned from their tar streams (`no-toc` or `toc-error`) get saved
file lists for the next pull. Layers built from their tables do not; a
later pull needs to fetch their tables again.

A closing log line sums the figures for the pull.

### Storage and source requirements

Only the delta path checks whether the missing file bytes fit on the
store's filesystem. It checks after reading tables and unpacking layers
without usable tables, before fetching the remaining missing files.
Insufficient space aborts the pull.

That estimate is a lower bound. The composefs image, and fs-verity trees
when enabled, need space too.

A delta pull requires a source that serves byte ranges. Registry sources,
layout directories, and local archives provide this interface. An
embedder's `BlobSource` must also implement `OpenRange`; otherwise, pull
fails with `ErrNoRangeSource` instead of silently fetching whole layers.
The library's delta option is available through `Pull` only. `NewBuild`
rejects it.

## Inspect What Landed

Look at the files written under the store root:

```sh
find /var/lib/ocicfs -maxdepth 5 -type f | head
# .../images/sha256/<aa>/<rest>/linux-amd64.json
# .../objects/sha256/<aa>/<rest>
```

Pull writes decompressed file content, the verified metadata chain, and
the composefs EROFS image into the store, with fs-verity when the build
runs with it. It also writes
the image binding and reference indexes.

Files of 64 bytes or fewer are inlined into the EROFS image. Larger files
are shared store objects.

Compressed layer bytes are not persisted. A delta pull may spool a whole
blob to a store temporary file until the pull ends.

See [on-disk-layout.md](on-disk-layout.md) for the full tree and JSON
schemas.

## Sealing Verification

Pull a sealed image with the usual `ocicfs pull` command. Verification is
automatic. See [seal.md](seal.md) for publishing a sealed image.

If the resolved manifest carries sealing annotations, pull compares them
with the content it built and measured. A mismatch aborts the pull without
recording a mismatched image. This check also runs when the image is
already stored.

### Choosing the expected image digest

Pull and mount choose the expected digest from the manifest alone, never
from how the image came out:

- Use `composefs.merged.erofs.v1.fsverity-sha256-12` when present.
- Otherwise, use
  `io.foundries.ocicfs.composefs.fallback.v1.fsverity-sha256-12`.
  `ocicfs seal --allow-non-canonical` writes this fallback annotation.

The chosen value must exactly match the merged image's measured fs-verity
digest. A malformed value also aborts the pull. If both annotations are
present, the standard one decides; the fallback is not read.

If the manifest's only annotation under `composefs.merged.erofs.v1.` has
an unrecognized kind, pull cannot verify its seal and fails. It does not
treat the image as unsealed. An image without any merged-image annotation
is a valid unsealed pull.

### Canonical and fallback construction

A construction that follows the standard rules and reproduces the
standard sealed digest is **canonical**. A **non-canonical** construction
instead produces its own deterministic fallback digest.

Pull rejects a non-canonical construction if the manifest carries any
annotation under `composefs.merged.erofs.v1.`, regardless of its suffix or
value. Such a construction cannot claim the reproducible digest promised
by that namespace. Its fallback annotation records its own digest.

The config descriptor's annotation follows the construction:

- Canonical: `composefs.config.fsverity-sha256-12`.
- Non-canonical:
  `io.foundries.ocicfs.composefs.config.fsverity-sha256-12`.

Pull checks the applicable annotation against the stored config's
measured fs-verity digest. A malformed value or mismatch aborts the pull.

ocicfs follows the upstream annotation keys and meanings defined in the
[composefs OCI sealing specification][sealing-spec].

[sealing-spec]: https://github.com/composefs/composefs-rs/blob/main/crates/composefs-oci/src/sealing_spec.rs

## Embedding: Feeding Layers as They Arrive

Use `Pull` to process an image in one call. Use `NewBuild` when your
runtime or agent receives layers incrementally:

```go
b, err := eng.NewBuild(ctx, ref, ocicfs.WithBlobSource(src), ocicfs.WithResolvedRoot(root))
if err != nil {
	return err
}
defer b.Close()
for i := range layers {
	// ... once layer i is in src ...
	if err := b.Ingest(ctx, i); err != nil {
		return err
	}
}
img, err := b.Finish(ctx)
```

This lets a runtime's snapshotter process each layer as it arrives.
The final step handles the last layer and image write instead of the
whole conversion.

`NewBuild` takes the same reference and options as `Pull`, except for the
[delta option](#storage-and-source-requirements). It resolves the
reference, verifies the metadata chain, and checks whether the store
already holds the image.

`Finish` processes remaining layers, builds and records the image, and
returns it. Plain `Pull` is `NewBuild` followed by `Finish`. Both store the
same objects, bindings, and reference records; only the reference
timestamp differs.

### Layer order and background work

The first `Ingest` or `Finish` starts background scans ahead of the
caller's requests. `Config.IngestWorkers` caps scans across all builds of
the engine; 0 selects the default of 2.

The build retains values from the `NewBuild` context but not its
cancellation. It can therefore span request handlers whose contexts end
on return. Scans run until `Finish` or `Close` stops them. A layer not yet
available from the source is retried until the caller asks for it.

`Ingest(i)` waits until every layer through `i` is merged in manifest
order. Jumping forward includes all intervening layers. An index already
ingested returns immediately. For an image already stored when
`NewBuild` ran, `Ingest` does nothing.

A layer can be merged from a scan or a
[saved file list](#layers-already-read). `Finish` checks whether the files
the final image needs are still present. It reads a layer whole if a
needed file is missing, so the source must still provide that layer at
`Finish`.

### Cancellation and failures

- A cancelled `Ingest` context returns its error but leaves the build
  running. You can repeat the call.
- A layer verification failure fails the build. Later calls return the
  same error, and no image is recorded. Because scans run ahead, a failure
  in layer `j` can reach a blocked `Ingest(i)` where `i < j`.
- Calls after `Finish` or `Close` return an error.
- `Ingest` and `Finish` run one at a time. A concurrent call waits for the
  current call. Only `Close` interrupts them.
- `Close` cancels all scans and waits for them. A blocked `Ingest` or
  `Finish` is interrupted and returns an error.
- Once `Finish` has merged every layer, `Close` waits for it instead.
  This includes reading a layer needed by a saved list and writing the
  image. Cancel the context passed to `Finish` to end such a read.
- `Finish` waits for every scan before returning, on success or failure.

Always close a build that will not finish. It holds the store against
`gc` from `NewBuild` until `Finish` or `Close` returns, including download
time. During that lifetime, `gc` reports `ErrStoreBusy`. Already sealed
objects remain for reuse or later collection, as described in
[Notes](#notes).

### Letting the runtime own image lifetime

Pass `ocicfs.WithHold(holder)` when your runtime owns the image's lifetime.

After writing the binding, `Finish` takes a hold on the image's identity
digest before releasing the store to `gc`. It records `ref` as a
**runtime reference**: a name that resolves for `inspect`, `mount`, `hold`,
and `release`, but does not itself keep the image alive.

Once the holder is released, `gc` drops that runtime reference. It also
reclaims the image if nothing else wants it. `ocicfs ls --format json`
shows the reference's `holder`.

A runtime reference never replaces a reference from a plain pull with the
same name. In that case, the image is held but not named. A later plain
pull of the name replaces a runtime reference and keeps the image alive
through its own reference.

An invalid holder name fails `NewBuild` before anything is fetched.

## Notes

- **Repeat or retry a pull normally.** Objects are keyed by content and
  committed without replacement. Existing content is recognized before
  sealing, so when fs-verity is enabled only new files pay the sealing
  cost.
  A delta pull of an already stored image is also a cache hit. After an
  interrupted pull, written objects remain until the next `gc`; a retry
  reuses those still present.
- **Concurrent pulls are safe but may duplicate work.** Pulls hold the
  store lock shared. There is no per-target lock. `gc` inventories under
  a shared lock, then takes it exclusively to decide and delete. It
  cannot delete content while a pull stages an image. See
  [gc.md](gc.md#concurrency).
- **Mount separately.** Pull prepares the store. Use `ocicfs mount` to
  mount the image; see [mount.md](mount.md).
- **Verification is part of pull and mount.** There is no standalone
  `verify` command.
