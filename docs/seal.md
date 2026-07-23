# Sealing an image (`knarr seal`)

`knarr seal` is a publisher-side command. For a digest-pinned image manifest
it computes the composefs integrity digests and records them as annotations,
then pushes the annotated manifest back to the same repository. It
reconstructs the image's merged composefs image exactly as a device would at
pull time, measures its fs-verity digest, and writes two annotations:

- `composefs.merged.erofs.v1.fsverity-sha256-12` on the image manifest — the
  fs-verity digest of the merged EROFS.
- `composefs.config.fsverity-sha256-12` on the manifest's config descriptor —
  the fs-verity digest of the config JSON.

Both values are recorded as bare lowercase hex.

```sh
digest=$(crane digest ghcr.io/example/app:1.2.3)
knarr seal "ghcr.io/example/app@$digest"
```

The reference must name an image manifest directly: a multi-platform image
index is declined, so an index's platform is sealed by pinning that child
manifest's own digest. `--tag <name>` also tags the sealed root in the input
repository. `--dry-run` computes the sealed digest without pushing.
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
reference form in the text rows). The reference is the form a follow-up command
consumes. Under `--dry-run` nothing is pushed, so the sealed reference names the
manifest the run *would* have published.

## Prerequisites

- `mkcomposefs` on `PATH` (from the composefs tools).
- A digest-pinned reference the registry serves, and push access to its
  repository.

## What seal computes

Seal resolves the pinned reference, streams the image's verified layers
through the same entry builder the device uses, writes the merged EROFS with
`mkcomposefs`, and measures its fs-verity digest in userspace. The manifest is
edited as a raw JSON document, so unknown fields on it and its descriptors
survive untouched. Resealing an already-sealed image is a no-op that keeps
the original bytes; a recorded annotation that disagrees with the recomputed
digest is an error.

## Non-canonical images are declined

Seal only records a digest it can vouch for as upstream-reproducible. It
refuses an image whose composefs construction diverges from the canonical form
— for example one whose layers omit `/usr` or a file's parent directory —
because such a tree's merged digest is not a stable value to seal. A declined
seal pushes **nothing**, including under `--tag`. The decline is reported on
stderr with a non-zero exit and is independent of `--dry-run` — a dry run of
an ineligible image is declined, not priced.

## What a call can target

The target of a call must be a container image manifest: it carries a config
and its layers are tar archives. That shape test — not the media type — is
what tells an image from a buildx attestation manifest, which shares the
image-manifest media type and carries a regular image config, and only its
layer types (in-toto JSON, not tar) reveal that it holds no filesystem to
seal. Naming an attestation manifest is an error.

## Authority boundary

Sealing establishes **publisher-side consistency**: it proves the recorded
annotations match the bytes the publisher pushed. It is not, by itself, a trust
anchor. Origin trust comes from the digest-pinned reference the upper-layer agent
chose and verifies; seal rides that same pinned chain. Native TUF/cosign
verification is a later addition. On the device, a pull of a sealed image
recomputes these digests and fails closed on a mismatch, so a tampered or
non-canonical sealed image is rejected at pull time.
