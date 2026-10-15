# Sealing an image (`ocicfs seal`)

`ocicfs seal` is a publisher-side command that seals **one platform per call**.
For a digest-pinned image manifest it computes the composefs integrity digests
and records them as annotations, then pushes the annotated manifest back to
the same repository. It reconstructs the platform's merged composefs image
exactly as a device would at pull time, measures its fs-verity digest, and
writes two annotations:

- `composefs.merged.erofs.v1.fsverity-sha256-12` on the image manifest — the
  fs-verity digest of the merged EROFS.
- `composefs.config.fsverity-sha256-12` on the manifest's config descriptor —
  the fs-verity digest of the config JSON.

Both values are recorded as bare lowercase hex. The annotations are defined
by the upstream
[composefs OCI sealing specification](https://github.com/composefs/composefs-rs/blob/main/crates/composefs-oci/src/sealing_spec.rs),
which ocicfs follows verbatim.

```sh
digest=$(crane digest ghcr.io/example/app:1.2.3)
ocicfs seal "ghcr.io/example/app@$digest"
```

When the reference names a multi-platform **image index**, `--platform`
selects the one child to seal (`os/arch[/variant]`, matched exactly against
the index descriptors — an index can carry several children differing only in
variant) and is required. The selected child's manifest is sealed and its
descriptor rewritten in the index; every other child — other platforms,
attestation manifests, vendor artifacts — is left byte-identical and never
even fetched. The printed sealed digest is the **updated index's**, so a whole
index is sealed by chaining calls, each feeding the previous output digest:

```sh
s1=$(ocicfs seal "ghcr.io/example/app@$digest" --platform linux/amd64 --format json | jq -r .sealed_ref)
s2=$(ocicfs seal "$s1" --platform linux/arm64 --format json | jq -r .sealed_ref)
```

A partially sealed index is a valid intermediate state: an unsealed platform
simply pulls as unsealed. `--tag <name>` also tags the sealed root in the
input repository. `--dry-run` computes the sealed digest without pushing.
`--format json` prints machine-readable output, including the platform's own
manifest digest before and after sealing; the text output is three aligned
rows:

```
original  ghcr.io/example/app@sha256:…
sealed    ghcr.io/example/app@sha256:…
fsverity  sha256:…
```

Every digest is reported twice: bare, and as the reference naming it in the
repository seal pushed to (`original_ref` / `sealed_ref`, and
`original_manifest_ref` / `sealed_manifest_ref` per platform in JSON; the
reference form in the text rows). The reference is the form the next chained
call consumes. Under `--dry-run` nothing is pushed, so the sealed reference
names the manifest the run *would* have published.

For an index the top-level digests are the **index's**, so the per-platform
`original_manifest` and `sealed_manifest` are the only report of what happened
to the selected child: equal values mean that platform was already sealed and
its descriptor was left alone.

## Prerequisites

- `mkcomposefs` on `PATH` (from the composefs tools).
- A digest-pinned reference the registry serves, and push access to its
  repository (with `--from-dir` the reads come from a local layout instead,
  and a `--from-dir` dry run needs no registry at all).

## What seal computes

Seal resolves the pinned reference — through the index to the selected
platform when one is given — streams the platform's verified layers through
the same entry builder the device uses, writes the merged EROFS with
`mkcomposefs`, and measures its fs-verity digest in userspace. Manifests are
edited as raw JSON documents, so unknown fields on the manifest, the index,
and their descriptors survive untouched. Resealing an already-sealed platform
is a no-op that keeps the original bytes; a recorded annotation that disagrees
with the recomputed digest is an error.

## Non-canonical images are declined

Seal only records a digest it can vouch for as upstream-reproducible. It
refuses an image whose composefs construction diverges from the
[canonical form](https://github.com/composefs/composefs-rs/blob/main/crates/composefs-oci/src/design.rs)
— the construction upstream's OCI generation rules define, for example one
whose layers omit `/usr` or a file's parent directory — because such a tree's
merged digest is not a stable value to seal. A declined seal pushes
**nothing**, including under `--tag`. The decline is reported on
stderr with a non-zero exit and is independent of `--dry-run` — a dry run of
an ineligible image is declined, not priced. The decline is per platform: a
non-canonical platform does not block sealing its canonical siblings in their
own calls.

## Sealing non-canonical content

`--allow-non-canonical` seals what the decline above would refuse: scratch
images and OCI artifacts delivered as tar layers — ML models, data bundles —
whose trees carry no `/usr` for the
[canonical transforms](https://github.com/composefs/composefs-rs/blob/main/crates/composefs-oci/src/design.rs)
to work from. ocicfs builds such trees with a deterministic fallback
construction, and the option records that fallback digest under ocicfs's own
annotations:

- `io.foundries.ocicfs.composefs.fallback.v1.fsverity-sha256-12` on the
  manifest, and
- `io.foundries.ocicfs.composefs.config.fsverity-sha256-12` on the config
  descriptor.

The shared `composefs.merged.erofs.v1.*` namespace is never used for a
fallback digest — it promises a value the upstream canonical algorithm
reproduces — and a non-canonical manifest already carrying a standard merged
annotation is refused even with the option. A canonical construction is
sealed under the standard annotations regardless of the option, so passing it
unconditionally is safe. On the device, pull verifies the fallback annotation
against the digest it re-derives and fails closed on a mismatch — the same
integrity story as a canonical seal, minus cross-tool reproducibility: the
reference tooling cannot rebuild fallback constructions, so the crosscheck
script does not apply to them. The target must still be an image manifest
with tar-archive layers.

## What a call can target

The target of a call must be a container image manifest: it carries a config
and its layers are tar archives. That shape test — not the media type — is
what tells an image from a buildx attestation manifest, which shares the
image-manifest media type and carries a regular image config, and only its
layer types (in-toto JSON, not tar) reveal that it holds no filesystem to
seal. Selecting such a child, or naming one directly, is an error. A selected
child that is itself an index is declined: recursive sealing is not supported.

## Sealing from a local image layout

`--from-dir <dir>` reads the image from a local OCI image layout directory
instead of pulling it from the registry. Straight from a build, when the host
already holds every byte seal needs:

```sh
docker buildx build --provenance=false -o type=oci,tar=false,dest=./layout .
crane push ./layout ghcr.io/example/app:1.2.3
digest=$(crane digest ghcr.io/example/app:1.2.3)
ocicfs seal --from-dir ./layout "ghcr.io/example/app@$digest"
```

Or export an already-published image with `crane pull`. With `--platform`
the layout roots at that platform's manifest, so the digest to pin is the
layout's own index entry:

```sh
crane pull --platform linux/amd64 --format=oci ghcr.io/example/app:1.2.3 ./layout
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
ocicfs seal --from-dir ./layout "ghcr.io/example/app@$digest"
```

`docker save` piped through `tar` is a third source when the daemon runs the
containerd image store (`docker info` reports `io.containerd.snapshotter.v1`):
it exports the registry's exact index, so a multi-arch image needs
`--platform` and holds only the platforms the daemon pulled. The legacy
graphdriver store instead reconstructs manifests whose digests match nothing
published — don't seal from those:

```sh
docker save ghcr.io/example/app:1.2.3 | tar -x -C ./layout
digest=$(jq -r '.manifests[0].digest' ./layout/index.json)
ocicfs seal --from-dir ./layout "ghcr.io/example/app@$digest" --platform linux/amd64
```

Only the reads move local: the annotated manifests are still pushed to the
repository the reference names, and the registry accepts them only when it
already holds the blobs they reference — so the flow is build, push, then
seal. A never-pushed image cannot be sealed into a registry this way, because
seal pushes manifests, not layers. The layout must contain the pinned root
and everything the seal reads through it — for an index target, the index
and the selected child. Watch what roots the layout: without
`--provenance=false` buildx attaches an attestation and the root becomes an
index, so sealing it needs `--platform`; `crane pull --format=oci` exports
the full index unless `--platform` narrows it to one child. With `--dry-run`
the command is fully offline.

## Local testing

The dev container serves zot, a real OCI registry, at `127.0.0.1:5000`, and
keeps the workspace `bin/` directory on `PATH`, so after `make build` the
examples' bare `ocicfs` resolves to the freshly built binary and sealing is
testable end to end by hand without external push access (busybox is a
multi-arch index, so the platform must be selected):

```sh
crane copy docker.io/library/busybox:latest 127.0.0.1:5000/app
digest=$(crane digest 127.0.0.1:5000/app:latest)
ocicfs seal "127.0.0.1:5000/app@$digest" --platform linux/amd64
ocicfs pull "127.0.0.1:5000/app@<sealed digest>"
```

The registry persists for the container's lifetime (storage under
`/var/lib/zot`, log at `/var/tmp/zot.log`). `make smoke` runs the same round
automatically — dry-run parity, the annotation check on the raw pushed
manifest, then a device-side pull of the sealed image — against its own
ephemeral zot instance, so smoke runs never see manual state.

The failure path is worth exercising by hand too: a sealed manifest whose
annotation disagrees with its content must not pull. Overwrite the recorded
merged digest and pull the result — `crane mutate` pushes the tampered
manifest under its own new digest and prints its reference (resealing is a
no-op, so rerunning seal just reports the sealed manifest's reference):

```sh
mref=$(ocicfs seal "127.0.0.1:5000/app@$digest" --platform linux/amd64 \
        --format json | jq -r '.platforms[0].sealed_manifest_ref')
tampered=$(crane mutate "$mref" \
        --annotation composefs.merged.erofs.v1.fsverity-sha256-12=$(printf '%064d' 0))
ocicfs pull "$tampered"
```

The pull materializes the content, measures it, finds the recorded annotation
disagrees, and fails closed without recording the image. The e2e suite runs
the same tamper check automatically in both directions — against a fresh pull
and against a restart over an already-stored image.

`scripts/seal-crosscheck.sh <image-uri>` verifies the sealed digest for any
image (current platform of an index) against three independent implementations:
`cfsctl oci compute-id` must reproduce it bit-for-bit, the Rust `mkcomposefs`
(`cfsctl mkcomposefs --from-file`, the drop-in for the C binary ocicfs runs)
must rebuild the sealed image's own dump into the identical bytes, and a fourth
build from an `umoci` flatten through the C `mkcomposefs` must agree at least on
the full tree (bit-for-bit when the image carries no hardlinks — see the script
header).

## Authority boundary

Sealing establishes **publisher-side consistency**: it proves the recorded
annotations match the bytes the publisher pushed. It is not, by itself, a trust
anchor. Origin trust comes from the digest-pinned reference the upper-layer agent
chose and verifies; seal rides that same pinned chain. Native TUF/cosign
verification is a later addition. On the device, a pull of a sealed image
recomputes these digests and fails closed on a mismatch, so a tampered or
non-canonical sealed image is rejected at pull time.
