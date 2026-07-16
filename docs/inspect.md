# Inspecting a local image (`knarr inspect`)

`knarr inspect` reports the OCI identity of an image already in the local store and the
composefs image built for it. It reads only local state and never touches the network.

```sh
knarr inspect app@sha256:<digest>
```

The reference takes the same forms `mount` accepts — `name@sha256:<hex>`, a bare
`sha256:<hex>`, or any ref string a previous pull recorded (a tag resolves locally to the
digest its most recent pull recorded, never over the network). It prints, for the host
platform:

```
root       sha256:<digest>          # the pinned root (index or manifest)
platform   linux/arm64/v8           # the resolved platform (config-declared)
composefs  sha256:<digest>          # the composefs image blob in the store
fsverity   sha256:<digest>          # the fs-verity digest a mount enforces
```

Per-layer detail — including the diff_ids — is shown by
[`--layers`](#per-layer-view---layers); the JSON output always carries the
`diff_ids` list as part of its stable contract.

Use `--format json` for machine-readable output:

```sh
knarr inspect app@sha256:<digest> --format json
# {"root":"sha256:…","platform":"linux/arm64/v8","diff_ids":["sha256:…"],
#  "composefs":{"mediaType":"…+erofs","digest":"sha256:…","size":12288},
#  "fsverity":"sha256:…"}
```

## Per-layer view (`--layers`)

`--layers` appends a table describing each filesystem layer of the resolved
manifest, in manifest order. It is assembled entirely from the locally stored,
digest-verified manifest and config — no layer body is read, and nothing is
fetched:

```sh
knarr inspect --layers app@sha256:<digest>
```

```
root       sha256:…
platform   linux/arm64/v8
composefs  sha256:…
fsverity   sha256:…

#  BLOB DIGEST   DIFFID        CHAINID       ARCHIVE  MEDIA TYPE
0  ab12cdef3456  cd34ab9012ef  cd34ab9012ef  3.4 MiB  application/vnd.oci.image.layer.v1.tar+gzip
1  ef5678901234  781290abcdef  90123456cdab  6.1 MiB  application/vnd.oci.image.layer.v1.tar+zstd
```

Column semantics:

- **ARCHIVE** is the layer blob's size from the manifest descriptor — the
  archive as transferred, compressed for gzip/zstd layers (OCI `+gzip`/`+zstd`
  media types or their Docker-schema equivalents) and the bare tar size for
  uncompressed ones. It is *not* the extracted filesystem size, which is not
  recorded in OCI metadata.
- **DIFFID** is the digest of the layer's *uncompressed* tar stream, from
  `config.rootfs.diff_ids`.
- **CHAINID** is the cumulative identity of the layer stack up to that layer,
  derived from the ordered diff_ids per the OCI image spec.
The table abbreviates digests to 12 hex characters (the media type prints
whole); `--format json` carries the full values, with the plain diff_id list
unchanged and a `layers` array added (the `layers` key appears only with
`--layers`).
The JSON layer entries additionally carry `createdBy` — the config history
command correlated to the layer (history entries marked `empty_layer` describe
config-only steps and consume no layer; when history is absent or does not
correlate cleanly, the field is omitted rather than guessed):

```sh
knarr inspect --layers --format json app@sha256:<digest>
# {…,"layers":[{"index":0,"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",
#   "digest":"sha256:…","diffID":"sha256:…","chainID":"sha256:…",
#   "size":3565158,"createdBy":"ADD rootfs.tar /"}]}
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
