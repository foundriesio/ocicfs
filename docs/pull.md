# Pulling images (`ocicfs pull`)

`ocicfs pull` materializes an OCI image into the local store: it verifies the
descriptor chain, writes the decompressed per-file content and the metadata
chain into the fs-verity-sealed object store, builds the composefs image, and
records the image binding and reference.

By default, ocicfs pulls from an OCI registry. `--from-dir` sideloads from a local
OCI image layout directory and `--from-tar` from an archive written by
`ocicfs copy`, `docker save` or skopeo instead, for air-gapped or test workflows. A layer
with a usable saved file list from an earlier pull is not read again (see
[Layers Already Read](#layers-already-read)). `--delta` fetches only the files
the store lacks from zstd:chunked layers, for updates
(see [Delta Pull](#delta-pull)).

## Prerequisites

- A store, normally on an **fs-verity-capable filesystem** (e.g. ext4 made with
  `-b 4096 -O verity`). The default build writes objects without fs-verity when
  the filesystem cannot enable it, and a `required` build fails fast instead,
  before anything is written (see
  [on-disk-layout.md](on-disk-layout.md#the-fs-verity-mode)).
- `mkcomposefs` on `PATH` (from the composefs tools).
- A registry reference (digest-pinned for production, tagged for testing), or a
  local OCI image layout and a digest-pinned reference when using `--from-dir`
  or `--from-tar`.

## Registry Pull

In production, pull a digest-pinned reference — the pinned digest is the trust
anchor, chosen and verified by whatever drives ocicfs:

```sh
digest=$(crane digest ghcr.io/example/app:1.2.3)
ocicfs pull "ghcr.io/example/app@$digest"
```

The store defaults to `/var/lib/ocicfs`; pass `--root <dir>` to use another
location. On success, ocicfs prints the ref and stored digest:

```
ghcr.io/example/app@sha256:<digest>	sha256:<digest>
```

Use `--format json` for machine-readable output:

```sh
ocicfs pull "ghcr.io/example/app@$digest" --format json
# {"ref":"ghcr.io/example/app@sha256:...","digest":"sha256:...","size":12288}
```

For testing, a tag works too:

```sh
ocicfs pull ghcr.io/example/app:1.2.3
# ghcr.io/example/app:1.2.3	sha256:<resolved digest>
```

A tag is a mutable alias, not a trust anchor: ocicfs resolves it to the digest
the registry currently serves (one extra round-trip) and from there runs the
same digest-verified pull as a pinned reference. The resolution step trusts the
registry, so pin digests in production — the printed digest is the one to pin.
The tag is recorded only as a reference alias; the image is stored under its
digest. Mount, inspect, and remove accept the same reference afterwards: an
unpinned ref resolves locally — never over the network — to the digest its most
recent pull recorded.

## The Printed Digest Is the Image Identity

A tag usually resolves to an image *index*, which lists one manifest per
platform. ocicfs keys an image by the manifest for the host platform, descending
one level inside the index's own digest-verified bytes to find it, so the digest
`pull` prints is that manifest's — the image's **identity** — and not
necessarily the digest the reference pinned:

```sh
ocicfs pull ghcr.io/example/app:1.2.3
# ghcr.io/example/app:1.2.3	sha256:<the host platform's manifest>
```

That is the digest to pin, to pass to `hold`, and to look the image up by.
Pulling the same image by its index digest and by its platform manifest digest
stores it once, under that one key, with one set of holds. The index digest the
reference landed on is kept as provenance in the reference record's `resolved`
field (see [on-disk-layout.md](on-disk-layout.md)); a later `pull`, `mount`,
`inspect`, `hold`, or `release` given the index digest descends it again and
lands on the same image.

Registry pull uses HTTPS, with one exception: a loopback registry (localhost
or a loopback IP) may use plain HTTP — content integrity comes from the
digest, not the channel (for a tag, the resolution itself trusts the loopback
channel). There is no insecure-registry option for any other host. Pull
currently emits no progress output.

Pull scans layers concurrently: `--ingest-workers N` sets how many layers are
scanned at once (default 2). On a registry pull each layer is streamed from the
registry as it is scanned, so N is also the number of concurrent blob
downloads. The cap is per engine, shared by every build that engine runs; a
`ocicfs pull` run beside a daemon adds its own workers to the daemon's. Scanning
is bound by the store's write and fs-verity sealing throughput, so more than
the default helps only on fast storage; on an SD card or eMMC the default is
the right setting.

## Local OCI Layout Sideload

Any tool that writes a standard OCI image layout works. For example, with
[crane](https://github.com/google/go-containerregistry):

```sh
crane pull --format=oci ghcr.io/example/app:1.2.3 ./layout
```

This yields `./layout/` containing `oci-layout`, `index.json`, and
`blobs/sha256/`. Pin a digest present in the layout:

```sh
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
ocicfs pull --from-dir ./layout "app@$digest"
```

The layout's `index.json` may name an index rather than a manifest; pinning it
is fine either way, since the pull descends to the host platform's manifest and
prints that identity.

`--ingest-workers N` applies to a sideload exactly as to a registry pull: the
layers are read from the layout directory and scanned N at a time (default 2).

The layers of a sideloaded image may be gzip, zstd or uncompressed tar: only
`--delta` needs zstd:chunked layers, and a sideload with `--delta` reads a
layer without a table of contents whole.

### From a `docker save` or `skopeo` archive

`docker save` and `skopeo copy` to an `oci-archive:` write an OCI image layout
as an uncompressed tar. `--from-tar` pulls from such an archive in place —
nothing is extracted:

```sh
docker save --platform linux/arm64 -o app.tar ghcr.io/example/app:1.2.3
ocicfs pull --from-tar app.tar
```

Without a reference, ocicfs records the name the archive carries, with its
tag or digest replaced by the digest of the single `index.json` entry — here
`ghcr.io/example/app@<manifest digest>`. The name is the reference
`docker save` was given, kept in the entry's `io.containerd.image.name`
annotation, or else a fully qualified reference in its
`org.opencontainers.image.ref.name` annotation, where skopeo keeps the
reference given after the archive path:

```sh
skopeo copy --preserve-digests docker://ghcr.io/example/app:1.2.3 \
  oci-archive:app.tar:ghcr.io/example/app:1.2.3
ocicfs pull --from-tar app.tar
```

A `ref.name` that is not fully qualified, such as the bare tag `docker save`
also writes there, names no image: an archive without either name needs a
reference.

The recorded name identifies what the archive holds; it does not authorize
it. An agent that wants a release authorized by its own trusted metadata
passes the digest from there instead:

```sh
ocicfs pull --from-tar app.tar "ghcr.io/example/app@$digest"
```

The pull fails unless the archive holds that digest, and any seal its
manifest carries is verified against the image the pull builds, so the pinned
digest authenticates the files that land. An archive whose manifest was
rewritten cannot match the pin: a `docker save` from a daemon on the legacy
graphdriver image store reconstructs a manifest whose digest matches nothing
published. Save the archive from a daemon on the containerd image store
(`docker info` reports `io.containerd.snapshotter.v1`), or with skopeo,
instead.

The reference may be `name@sha256:<hex>`, a bare `sha256:<hex>`, or a bare
name, which ocicfs pins to the one manifest the archive's `index.json` lists:

```sh
ocicfs pull --from-tar app.tar ghcr.io/example/app
```

Note that `docker save --platform` exports the platform's child manifest and
keeps the reference it was given in the name annotation, so an annotation may
name the index digest while the archive holds only the child: a pinned
reference must name a digest the archive actually holds. An archive holding
several images is refused with the digests and names of its entries; pin one
of them. An archive `ocicfs copy` wrote for one index image lists the index
and its platform manifest, and holds that one image: without a pin, ocicfs
records the index. The tag of a bare name is not recorded — the image is keyed by
`name@digest`, as with `--from-dir`. A gzip-, zstd-, xz- or bzip2-compressed
archive is refused: decompress it first. `--delta` and `--ingest-workers`
apply as to `--from-dir`.

### Writing an archive with `ocicfs copy`

`ocicfs copy` fetches images from their registry and writes them to one
archive that `--from-tar` and `docker load` read. It needs no store and no
privilege, so it runs on a build host or in CI: a factory build that ships
images to devices offline packs them there, with no docker daemon.

```sh
ocicfs copy --platform linux/arm64 -o images.tar \
  ghcr.io/example/app:1.2.3 ghcr.io/example/db@sha256:<digest>
```

On the device, sideload each image, pinned to the digest its compose file
names, then load the archive once:

```sh
ocicfs pull --from-tar images.tar "ghcr.io/example/app@$pin"
ocicfs pull --from-tar images.tar "ghcr.io/example/db@$pin"
docker load -i images.tar
```

The archive keeps the registry's bytes, so every digest and the publisher's
seal survive, and only the blobs of one platform, `--platform` (default: the
host's). An image whose root is an index is listed twice in `index.json`:
by the index, named by the reference it was copied by — its tag, or its digest
when the reference carries only a digest — and by the platform's manifest,
named by that manifest's digest. A pin on either digest then resolves, both
for `--from-tar` and for the image docker loads. Both name annotations,
`io.containerd.image.name` and `org.opencontainers.image.ref.name`, carry the
full name. Blobs the images share are written once. Every blob is verified as
it is copied, and the archive appears under its name only once complete. The
command prints each `index.json` entry it wrote, as `<name>\t<digest>`.

## Layers Already Read

A layer with a usable saved file list is not read again. When a pull has read
a layer's tar stream to its end and verified it — the blob against the digest
and size the manifest names, the unpacked stream against the `diff_id` the
config names — it saves the layer's file list in the store, named by the
blob digest (see [on-disk-layout.md](on-disk-layout.md)). A later pull of
any image whose manifest names the same blob digest with the same size, and
whose config names the same `diff_id` for it, merges the layer from that
file list without opening the blob: as long as the store holds the files the
image needs, nothing of the layer is downloaded, decompressed or hashed. The
layer is read again when the image needs a file of it the store no longer
holds, and when there is no usable file list: it was too large to write or
could not be written, `gc` swept it with the last image listing the layer,
or it does not decode. An update that keeps its lower layers, with their
file lists in place, then reads only the layers that changed:

```sh
ocicfs pull "ghcr.io/example/app@$previous"
ocicfs pull "ghcr.io/example/app@$next"   # reads only the layers $previous lacks
```

The file list names every stored file by its sha256 and size and carries the
bytes of the files small enough to be inlined into the image; the files
themselves are the store's objects. Once every layer is merged, the pull
looks up the files the merged image keeps, and only those: a file an upper
layer overwrites or removes is never asked for, even after `gc` reclaimed
it. When a file the image needs is absent — `gc` reclaimed it while an
earlier release shadowed it, and this release no longer does — the layer
that lists it is read whole, once, as if it had not been saved: every file
it carries is stored, and its blob digest and `diff_id` are verified again.
One case reads a layer for a file the image drops: files under `/run` are
removed only after the layers are merged, so an absent one still has its
layer read.

The image built is bit-identical to the one a pull into an empty store
builds, and a pull accepts and rejects the same image content either way.
One difference is intended: a pull that has no layer left to read succeeds
when the source no longer serves the blobs. The identity is the blob digest,
so a layer republished with the same content under another compression is
read once more. A saved file list that does not decompress to its end, was
written in another format version, holds a malformed record, exceeds the
size bounds, or was saved for another blob size or `diff_id` is ignored with
a warning, and the layer is read. A file list that cannot be saved, or is
too large to be, is a warning too, never a failed pull.

A saved file list is local state, relied on as the image binding is: on the
assumption that nothing but ocicfs writes to the store. Its checksum catches
corruption, not a deliberate change, and nothing re-checks it against the
`diff_id` afterwards. What a pull builds from it is checked against a value
the publisher pinned only for a sealed image (see
[Sealing Verification](#sealing-verification)): there the merged image's
fs-verity digest must equal the sealing annotation, at pull and again at
mount. **Only sealed images are protected against a modified store.** For
an unsealed image an altered file list is merged unnoticed, into that image
and into every later one that shares the layer.

A pull that fails in a way a saved file list can have caused removes the
file lists it used, so the retry reads those layers from the source: when
the layers do not merge, when a file one of them names cannot be had from
the layer it names it in, when the image cannot be built, or when the
sealing check fails. A source that cannot serve a layer and a cancelled pull
leave them in place.

This applies to a registry pull, to `--from-dir`, to an embedder's
`NewBuild`, and in a `--delta` pull to the layers without a usable table of
contents. `ocicfs gc` deletes the saved file lists of layers no wanted image
lists (see [gc.md](gc.md)).

## Delta Pull

An update usually changes a few files of an image, but every layer whose
digest changed is fetched whole, which for a rebuilt image is all of them.
`--delta` fetches, of every layer published in **zstd:chunked** format, only
its table of contents and the files the store does not already hold, as byte
ranges of the layer blob:

```sh
ocicfs hold --holder delta-base "ghcr.io/example/app@$previous"
ocicfs pull --delta "ghcr.io/example/app@$next"
# ... switch the workload over to $next ...
ocicfs release --holder delta-base "ghcr.io/example/app@$previous"
ocicfs remove "ghcr.io/example/app@$previous"
ocicfs gc
```

The hold is the point: a delta saves bytes only while the previous release's
files are still in the store, and `gc` reclaims an image nothing wants. The
agent driving ocicfs holds release N−1 under a holder of its own — `delta-base`
above — before pulling N, so that whatever else it does meanwhile (repointing
a tag, removing the old reference to make room) cannot cost the base, and
releases it once N is in place; a plain `remove` declines while the hold
stands, so the release comes first. ocicfs adds no keep-previous policy of its
own. The same sequence works with `--from-dir` against a layout directory.

What a delta can save is bounded by the share of file bytes the two releases
have in common (how much of that bound a pull realises then depends on the
compression, the coalescing of ranges and the fallback thresholds), and that
share is the publisher's to control. A rebuild that regenerates files —
compiled artifacts, gzip-compressed documentation, package updates — changes
their digests even when nothing meaningful changed. Measured on official
images: consecutive patch releases of the same lines (nginx, debian, python)
shared 69 % of their file bytes on average and 85 % in the median update,
while a release that also rebuilt its base image shared 7 % (two `python`
builds a month apart) or less than 1 %, so `--delta` saved nothing there. A
build pipeline that keeps unchanged files byte-identical from one release to
the next is what makes `--delta` pay.

What the image needs: zstd:chunked layers, as `skopeo copy
--dest-compress-format zstd:chunked` writes them. Such a layer is a
standard `tar+zstd` blob whose descriptor carries the
`io.github.containers.zstd-chunked.manifest-position` and `manifest-checksum`
annotations. The table is fetched as one range, authenticated by that
checksum, and every file by the digest the table records for it; the manifest
chain, sealing annotations and the composefs image are verified exactly as by
a plain pull, and the image built is bit-identical to a plain pull's — a
delta pull and a plain pull of the same image reference the same objects
and record the same binding. A plain pull also stores the files a later
layer shadows, which gc reclaims; a layer built from its table stores only
the files the image needs, though a layer that fell back to the tar stream
stores every file it carries, and a whole-blob read transfers the shadowed
bytes even if it does not store them. For a layer built from its table the `diff_id` is reported by `inspect` and the
chain IDs as before but no longer verified: nothing decompresses the whole
tar stream to hash it. A layer that fell back to the tar stream is verified
as a plain pull verifies it, and one merged from its saved file list was
verified so when the list was saved.

What falls back, and why, is reported by one log line per layer on stderr:

```text
level=INFO msg="delta: layer" layer=sha256:… size=1452 toc_bytes=381 toc_ms=3 entries=5 missing=2 planned=369 requests=1 fetched=750 fallback=none
```

`missing` counts the files the store lacked, `planned` and `requests` the
bytes and ranges those files would take as ranges (a layer served from a
whole answer to its table request asks for none and reports `requests=0`),
`fetched` the bytes the pull actually read for the layer, table included, and
`fallback` why the layer was not fetched by ranges:

- `no-toc` — a layer without a table (a plain `tar+gzip` layer, for example)
  is streamed and unpacked as a plain pull does.
- `toc-error` — the table could not be read or did not verify: the layer is
  streamed and unpacked as a plain pull does, after a warning naming the
  cause. The whole blob is verified against the manifest on that path, so a
  bad table can cost bandwidth but never integrity.
- `requests` or `fraction` — the missing files would take more than 256
  ranges, or more than half the blob: the blob is read once, in one request,
  and only the missing files are unpacked from it. A rebuilt base image
  trips this rule before the first request.
- `whole-answer` — the registry answered a range request with the whole blob
  (no `Range` support): the rest of the layer is read from that answer.
- `saved` — a layer without a usable table that an earlier pull read (see
  [Layers Already Read](#layers-already-read)): it is merged from its saved
  file list and reports `fetched=0`. A file the image needs, the store lacks
  and a table also lists is still fetched by range; one only the saved file
  list names has the layer read whole, and `fetched` is then the blob's
  size. A layer scanned from its tar stream (`no-toc`, `toc-error`) is saved
  for the next pull; a layer built from its table is not, and costs the next
  pull its table again.

A closing line per pull sums the same figures. Once the tables are read and
the layers without a usable table are unpacked, and before the remaining
missing files are fetched, the pull checks that the store's filesystem has
room for those files' bytes and refuses when it does not; that figure is a
lower bound, since the fs-verity trees and the composefs image come on top.

`--delta` needs a source that serves byte ranges: a registry and a layout
directory do; an embedder's own `BlobSource` only when its type also
implements `OpenRange`, and the pull fails with `ErrNoRangeSource` otherwise
rather than silently fetching whole layers. It is a `Pull` option only:
`NewBuild` refuses it.

## Inspect What Landed

Pull writes the object store plus the binding and reference indexes:

```sh
find /var/lib/ocicfs -maxdepth 5 -type f | head
# .../images/sha256/<aa>/<rest>/linux-amd64.json
# .../objects/sha256/<aa>/<rest>
```

See [on-disk-layout.md](on-disk-layout.md) for the full tree and JSON schemas.

## Sealing Verification

If a resolved manifest carries composefs sealing annotations, pull checks them
against the content it actually materialized and measured, and fails closed on
disagreement rather than recording a mismatched image. The same check runs on a
fresh pull and on the cached fast path, so a previously stored image cannot skip
it.

The merged-image annotation a pull holds the image to is chosen by what the
manifest carries, as `mount` chooses it, and never by how the image came
out: the standard `composefs.merged.erofs.v1.fsverity-sha256-12` when it is
present, else the ocicfs fallback annotation
(`io.foundries.ocicfs.composefs.fallback.v1.fsverity-sha256-12`, written by
`ocicfs seal --allow-non-canonical`). The chosen annotation must match the
measured fs-verity digest of the merged image exactly; a mismatch or a
malformed value aborts the pull. When both are present the standard one
decides and the fallback one is not read. A manifest whose only annotation
under the `composefs.merged.erofs.v1.` prefix is of a kind ocicfs does not
recognize is sealed in a way it cannot verify, and the pull fails rather
than treat the image as unsealed. An image carrying no merged-image
annotation is a valid unsealed pull.

A **canonical** construction reproduces the sealed digest the standard
annotation records. A **non-canonical** construction cannot honestly claim a
reproducible sealed digest under that namespace, so pull rejects it outright
if the manifest carries any annotation under the
`composefs.merged.erofs.v1.` prefix — regardless of the suffix or value it
records. What it does reproduce is its own deterministic fallback digest,
which is the value the fallback annotation records.

The config annotation, on the config descriptor, follows the construction:
`composefs.config.fsverity-sha256-12` is checked on a canonical
construction and `io.foundries.ocicfs.composefs.config.fsverity-sha256-12`
on a non-canonical one, against the measured fs-verity digest of the stored
config and with the same fail-closed handling. The upstream annotation keys
and their meaning are defined by the
[composefs OCI sealing specification](https://github.com/composefs/composefs-rs/blob/main/crates/composefs-oci/src/sealing_spec.rs),
which ocicfs follows verbatim.

## Embedding: Feeding Layers as They Arrive

`Pull` converts the whole image in one call. An embedder that receives an
image's layers one at a time — a container runtime unpacking a pull hands its
snapshotter each layer as it lands — can instead have ocicfs materialize each
layer as it arrives, so the last step does only the last layer plus the image
write rather than the whole conversion. The engine's `NewBuild` starts such a
pull and returns a `Build`:

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

`NewBuild` takes the same reference and options as `Pull`, resolves the
reference, verifies the metadata chain, and decides whether the store already
holds the image. From the first `Ingest` or `Finish` on, the build scans
layers ahead of the caller's calls in the background, under the engine's worker
cap. `Ingest(i)` blocks
until every layer up to and including `i` is merged, in manifest order — a
jump forward applies every layer in between, an index at or below one already
ingested returns at once, and on an image the store already held it does
nothing at all. `Finish` ingests whatever is left, builds and records the
image, and returns it exactly as `Pull` would; `Pull` itself is `NewBuild`
followed by `Finish`, so the two paths store the same objects, bindings and
reference records, differing only in the timestamp a reference record carries.

Rules the build enforces:

- **Layers are scanned ahead, under the cap.** The build keeps the values of
  the `NewBuild` context but not its cancellation, so it can span request
  handlers whose contexts end on return; the scans it starts run until
  `Finish` or `Close` stops them. A layer the blob source does not hold yet
  is retried until the caller asks for it. `Config.IngestWorkers` sets the
  cap for every build of the engine (0 means the default of 2).
- **`Ingest(i)` blocks until layer `i` is merged.** It returns once every
  layer up to `i` is merged, whichever scan got there first: read into the
  store, or merged from its saved file list without being read (see
  [Layers Already Read](#layers-already-read)). `Finish` then looks the
  files of such layers up in the store and reads a layer whole, through the
  blob source, when one the image needs is absent, so the source must still
  hold that layer at `Finish`. A cancelled context returns its error and
  leaves the build running; the call may be repeated.
- **Failures surface early, and stay.** A layer that fails verification
  fails the build: every later call returns the same error, and nothing is
  recorded. Because layers are scanned ahead, the failure of layer `j` may
  be reported by the `Ingest` of an earlier index `i < j` that was blocked
  meanwhile. Calls after `Finish` or `Close` return an error too.
- **`Close` cancels and waits.** It cancels every scan, waits for them, and
  interrupts an `Ingest` or `Finish` blocked meanwhile, which then returns
  an error. `Finish` drains every scan before returning, whether or not it
  succeeded. A `Finish` that has every layer merged is not interrupted:
  `Close` waits while it reads a layer for a saved file list and writes the
  image, and cancelling the context `Finish` was given is what ends such a
  read.
- **One call at a time.** `Ingest` and `Finish` are serialized: a call that
  arrives while another is in progress waits for it. Only `Close` cuts in,
  as above.
- **The store is held against `gc` until `Finish` or `Close` returns.** `gc`
  reports `ErrStoreBusy` from `NewBuild` until then, downloads included,
  rather than for the conversion alone. Always `Close` a build that will not
  `Finish`; objects it already sealed stay as unreferenced content for `gc`
  to reclaim.
- **`WithHold` keeps the image by a hold, and names it without pinning it.**
  An embedder that owns an image's lifetime itself — a runtime that drops the
  image when its own records go — passes `ocicfs.WithHold(holder)`: `Finish`
  takes a hold under `holder` on the image's identity digest, after the
  binding is written and before the store is released to `gc`, and records
  `ref` as a *runtime reference*: a name that resolves like any other for
  `inspect`, `mount`, `hold` and `release`, but keeps nothing alive — `gc`
  drops it once `holder` is released, along with the image if nothing else
  wants it. `ocicfs ls --format json` shows such a row with its `holder`. A
  runtime reference never displaces a reference a plain pull recorded for
  the same name (the image is then held but not named), while a plain pull
  of the name replaces the runtime reference and keeps the image on its
  own. A holder name the hold store rejects fails `NewBuild` before anything
  is fetched.

## Notes

- **Reference forms.** Registry pull accepts `name@sha256:<hex>` and `name:tag`
  (`:latest` when neither is given) and rejects bare digests, which name no
  repository to fetch from. The local `--from-dir` sideload is digest-pinned
  only — a tag cannot be resolved without a registry — and also accepts a bare
  `sha256:<hex>` because the OCI layout directory supplies the source.
  `--from-tar` accepts the same forms plus a bare name, pinned to the single
  manifest the archive's `index.json` lists, and no reference at all, taken
  from the name annotation of that entry.
- **Idempotent.** Objects are content-addressed and committed without replacement,
  so re-pulling the same image reuses existing content. Content the store already
  holds is recognized before fs-verity is enabled on it, so an update pays the
  sealing cost only for files that are new.
- **Mount is separate.** Pull only materializes content into the store; mounting
  it is a distinct step with `ocicfs mount`.
- **The delta-base contract.** `--delta` saves bytes only while the previous
  release's objects are in the store. The agent holds release N−1 under a
  holder such as `delta-base` before pulling N, and releases it after N is
  activated; ocicfs adds no keep-previous policy of its own, and a `gc` that
  finds N−1 unwanted reclaims it like any other image. A delta pull of an
  image the store already holds is a cache hit, as any pull is. For a
  zstd:chunked layer the `diff_id` is reported (`inspect`, the chain IDs,
  JSON) but no longer verified by a delta pull; the layer's files are
  verified by the digests its table of contents records, the table by the
  checksum the manifest carries.
