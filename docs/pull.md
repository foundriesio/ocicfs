# Pulling from a local OCI layout (`knarr pull --from-dir`)

`knarr pull` materializes a **digest-pinned** OCI image into the local store: it
verifies the descriptor chain, writes the decompressed per-file content and the
metadata chain into the fs-verity-sealed object store, builds the composefs image,
and records the image binding and reference. It fetches only content the store
does not already hold.

Today the source is a **local OCI image layout** directory (`--from-dir`), the
air-gapped sideload path. Registry pull is not wired yet.

## Prerequisites

- A store on an **fs-verity-capable filesystem** (e.g. ext4 made with `-b 4096`).
  knarr fails fast if the store root cannot enable fs-verity.
- `mkcomposefs` on `PATH` (from the composefs tools).
- A local **OCI image layout** directory to pull from.

## 1. Get an OCI layout

Any tool that writes a standard OCI image layout works. For example, with
[crane](https://github.com/google/go-containerregistry):

```sh
crane pull --format=oci ghcr.io/example/app:1.2.3 ./layout
```

This yields `./layout/` containing `oci-layout`, `index.json`, and `blobs/sha256/`.

## 2. Find the digest to pin

knarr requires a digest-pinned reference — a tag alone is refused, because the
pinned digest is the trust anchor. Pin the digest crane recorded in the layout,
so it always matches the manifest that landed on disk:

```sh
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
```

(For a single-platform image this equals `crane digest ghcr.io/example/app:1.2.3`.)

## 3. Pull

```sh
knarr pull --from-dir ./layout "app@$digest"
```

The store defaults to `/var/lib/knarr`; pass `--root <dir>` to use another location.

The name before `@` is recorded for provenance; the `sha256:<digest>` after it is
what knarr resolves and verifies. On success it prints the ref and stored digest:

```
app@sha256:<digest>	sha256:<digest>
```

Use `--format json` for machine-readable output:

```sh
knarr pull --from-dir ./layout "app@$digest" --format json
# {"ref":"app@sha256:…","digest":"sha256:…","size":12288}
```

## 4. Inspect what landed

Pull writes the object store plus the binding and reference indexes:

```sh
find /var/lib/knarr -maxdepth 5 -type f | head
# …/images/sha256/<aa>/<rest>/linux-amd64.json   ← the binding
# …/objects/sha256/<aa>/<rest>                    ← content objects
```

See [on-disk-layout.md](on-disk-layout.md) for the full tree and JSON schemas.

## Notes

- **Digest-pinned only.** Accepted forms are `name@sha256:<hex>` or a bare
  `sha256:<hex>`. A tag-only reference is rejected.
- **Idempotent.** Objects are content-addressed and committed without replacement,
  so re-pulling the same image transfers nothing new.
- **Mount is separate.** Pull only materializes content into the store; mounting it
  is a distinct step (not yet implemented).
