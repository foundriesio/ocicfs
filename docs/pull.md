# Pulling images (`knarr pull`)

`knarr pull` materializes an OCI image into the local store: it verifies the
descriptor chain, writes the decompressed per-file content and the metadata
chain into the fs-verity-sealed object store, builds the composefs image, and
records the image binding and reference.

By default, knarr pulls from an OCI registry. `--from-dir` sideloads from a local
OCI image layout directory instead, for air-gapped or test workflows.

## Prerequisites

- A store on an **fs-verity-capable filesystem** (e.g. ext4 made with `-b 4096`).
  knarr fails fast if the store root cannot enable fs-verity.
- `mkcomposefs` on `PATH` (from the composefs tools).
- A registry reference (digest-pinned for production, tagged for testing), or a
  local OCI image layout and a digest-pinned reference when using `--from-dir`.

## Registry Pull

In production, pull a digest-pinned reference — the pinned digest is the trust
anchor, chosen and verified by whatever drives knarr:

```sh
digest=$(crane digest ghcr.io/example/app:1.2.3)
knarr pull "ghcr.io/example/app@$digest"
```

The store defaults to `/var/lib/knarr`; pass `--root <dir>` to use another
location. On success, knarr prints the ref and stored digest:

```
ghcr.io/example/app@sha256:<digest>	sha256:<digest>
```

Use `--format json` for machine-readable output:

```sh
knarr pull "ghcr.io/example/app@$digest" --format json
# {"ref":"ghcr.io/example/app@sha256:...","digest":"sha256:...","size":12288}
```

For testing, a tag works too:

```sh
knarr pull ghcr.io/example/app:1.2.3
# ghcr.io/example/app:1.2.3	sha256:<resolved digest>
```

A tag is a mutable alias, not a trust anchor: knarr resolves it to the digest
the registry currently serves (one extra round-trip) and from there runs the
same digest-verified pull as a pinned reference. The resolution step trusts the
registry, so pin digests in production — the printed digest is the one to pin.
The tag is recorded only as a reference alias; the image is stored under its
digest. Mount, inspect, and remove accept the same reference afterwards: an
unpinned ref resolves locally — never over the network — to the digest its most
recent pull recorded.

## The Printed Digest Is the Image Identity

A tag usually resolves to an image *index*, which lists one manifest per
platform. knarr keys an image by the manifest for the host platform, descending
one level inside the index's own digest-verified bytes to find it, so the digest
`pull` prints is that manifest's — the image's **identity** — and not
necessarily the digest the reference pinned:

```sh
knarr pull ghcr.io/example/app:1.2.3
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
`knarr pull` run beside a daemon adds its own workers to the daemon's. Scanning
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
knarr pull --from-dir ./layout "app@$digest"
```

The layout's `index.json` may name an index rather than a manifest; pinning it
is fine either way, since the pull descends to the host platform's manifest and
prints that identity.

`--ingest-workers N` applies to a sideload exactly as to a registry pull: the
layers are read from the layout directory and scanned N at a time (default 2).

## Inspect What Landed

Pull writes the object store plus the binding and reference indexes:

```sh
find /var/lib/knarr -maxdepth 5 -type f | head
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

A **canonical** construction reproduces the sealed digest, so a present
`composefs.merged.erofs.v1.fsverity-sha256-12` (on the manifest) or
`composefs.config.fsverity-sha256-12` (on the config descriptor) annotation
must match the measured value exactly; a mismatch aborts the pull, and an
image carrying neither annotation is always a valid unsealed pull. A
**non-canonical** construction cannot honestly claim a reproducible sealed
digest under that namespace, so pull rejects it outright if the manifest
carries any annotation under the `composefs.merged.erofs.v1.` prefix —
regardless of the suffix or value it records. What it does reproduce is its
own deterministic fallback digest, so a present knarr fallback annotation
(`io.foundries.knarr.composefs.fallback.v1.fsverity-sha256-12`, written by
`knarr seal --allow-non-canonical`) is verified against the re-derived value
with the same fail-closed handling. A non-canonical image carrying neither is
likewise a valid unsealed pull. The upstream annotation keys and their
meaning are defined by the
[composefs OCI sealing specification](https://github.com/composefs/composefs-rs/blob/main/crates/composefs-oci/src/sealing_spec.rs),
which knarr follows verbatim.

## Embedding: Feeding Layers as They Arrive

`Pull` converts the whole image in one call. An embedder that receives an
image's layers one at a time — a container runtime unpacking a pull hands its
snapshotter each layer as it lands — can instead have knarr materialize each
layer as it arrives, so the last step does only the last layer plus the image
write rather than the whole conversion. The engine's `NewBuild` starts such a
pull and returns a `Build`:

```go
b, err := eng.NewBuild(ctx, ref, knarr.WithBlobSource(src), knarr.WithResolvedRoot(root))
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
  layer up to `i` is in the store, whichever scan got there first. A
  cancelled context returns its error and leaves the build running; the
  call may be repeated.
- **Failures surface early, and stay.** A layer that fails verification
  fails the build: every later call returns the same error, and nothing is
  recorded. Because layers are scanned ahead, the failure of layer `j` may
  be reported by the `Ingest` of an earlier index `i < j` that was blocked
  meanwhile. Calls after `Finish` or `Close` return an error too.
- **`Close` cancels and waits.** It cancels every scan, waits for them, and
  interrupts an `Ingest` or `Finish` blocked meanwhile, which then returns
  an error. `Finish` drains every scan before returning, whether or not it
  succeeded.
- **One call at a time.** `Ingest` and `Finish` are serialized: a call that
  arrives while another is in progress waits for it. Only `Close` cuts in,
  as above.
- **The store is held against `gc` until `Finish` or `Close` returns.** `gc`
  reports `ErrStoreBusy` from `NewBuild` until then, downloads included,
  rather than for the conversion alone. Always `Close` a build that will not
  `Finish`; objects it already sealed stay as unreferenced content for `gc`
  to reclaim.

## Notes

- **Reference forms.** Registry pull accepts `name@sha256:<hex>` and `name:tag`
  (`:latest` when neither is given) and rejects bare digests, which name no
  repository to fetch from. The local `--from-dir` sideload is digest-pinned
  only — a tag cannot be resolved without a registry — and also accepts a bare
  `sha256:<hex>` because the OCI layout directory supplies the source.
- **Idempotent.** Objects are content-addressed and committed without replacement,
  so re-pulling the same image reuses existing content.
- **Mount is separate.** Pull only materializes content into the store; mounting
  it is a distinct step with `knarr mount`.
