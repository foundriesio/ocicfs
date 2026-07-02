# Inspecting a local image (`knarr inspect`)

`knarr inspect` reports the OCI identity of an image already in the local store and the
composefs image built for it. It reads only local state and never touches the network.

```sh
knarr inspect app@sha256:<digest>
```

The reference is digest-pinned, the same forms `mount` accepts — `name@sha256:<hex>` or a bare
`sha256:<hex>`. It prints, for the host platform:

```
root       sha256:<digest>          # the pinned root (index or manifest)
platform   linux/arm64/v8           # the resolved platform (config-declared)
diff_id    sha256:<digest>          # one line per layer, in order
composefs  sha256:<digest>          # the composefs image blob in the store
fsverity   sha256:<digest>          # the fs-verity digest a mount enforces
```

Use `--format json` for machine-readable output:

```sh
knarr inspect app@sha256:<digest> --format json
# {"root":"sha256:…","platform":"linux/arm64/v8","diff_ids":["sha256:…"],
#  "composefs":{"mediaType":"…+erofs","digest":"sha256:…","size":12288},
#  "fsverity":"sha256:…"}
```

## Success and failure

`inspect` succeeds only when the image is fully materialized locally: its OCI metadata chain
(root → manifest → config) resolves from the store **and** a composefs binding is recorded for
the resolved `{root, platform}`. Otherwise it fails with an error satisfying
`errors.Is(err, knarr.ErrNotFound)` and exits non-zero, pointing at `knarr pull`.

- **Not pulled** (metadata or binding absent) → not-found; re-running `knarr pull` is a clean,
  idempotent remedy.
- **Wrong platform** — an image present only for a different platform is **not** reported as
  not-found; the error names the platform conflict instead.

`inspect` does not measure fs-verity or open the composefs object; it reports what the store
records. Mount is the enforcement point (see [mount.md](mount.md)); the binding is auxiliary,
verified on use.
