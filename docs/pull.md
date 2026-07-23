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

A tag is a mutable alias, not a trust anchor: knarr resolves it to the manifest
digest the registry currently serves (one extra round-trip) and from there runs
the same digest-verified pull as a pinned reference. The resolution step trusts
the registry, so pin digests in production — the printed digest is the one to
pin. The tag is recorded only as a reference alias; the image is stored under
its digest. Mount, inspect, and remove accept the same reference afterwards:
an unpinned ref resolves locally — never over the network — to the digest its
most recent pull recorded.

Registry pull uses HTTPS, with one exception: a loopback registry (localhost
or a loopback IP) may use plain HTTP — content integrity comes from the
digest, not the channel (for a tag, the resolution itself trusts the loopback
channel). There is no insecure-registry option for any other host. Pull
currently emits no progress output.

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
