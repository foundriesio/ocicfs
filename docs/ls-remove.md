# Listing and forgetting local images (`knarr ls`, `knarr remove`)

`knarr ls` lists the images in the local store: every pulled reference, then
every image a hold alone keeps. `knarr remove` forgets one exact reference from
the auxiliary indexes. It does not delete content objects; object reclamation
is handled by `gc`.

## Prerequisites

- A knarr store containing at least one pulled image.
- The exact reference string recorded by `knarr pull`.

## 1. List pulled images

```sh
knarr ls
```

Text output prints one row per reference:

```text
app@sha256:<digest>	sha256:<digest>	481
```

The columns are the exact stored reference, the image identity digest — the
platform manifest, the same digest `knarr pull` reports — and the size of the
image's composefs image object.

Use `--format json` for machine-readable output:

```sh
knarr ls --format json
# [
#   {"ref":"app@sha256:...","digest":"sha256:...","size":481}
# ]
```

An empty store prints no text rows, or `[]` in JSON, and exits successfully.

A reference a runtime pull recorded — one made with `WithHold`, the way a
container runtime keeps the images it lists — is a *runtime reference*: it
names the image for the runtime's holder and pins nothing, so `gc` drops it,
and the image, once that holder is released. It lists like any other row;
in JSON it carries the holder:

```sh
knarr ls --format json
# [
#   {"ref":"docker.io/library/nginx:latest","digest":"sha256:...","size":481,"holder":"cfs-build-..."}
# ]
```

`knarr remove` of such a reference declines while the holder stands, the
same as for any held image. An agent that wants the image kept adopts it by
pulling the name (`knarr pull nginx:latest`), which replaces the runtime
reference with its own, or by holding it.

An image that no reference names at all but a hold keeps is listed after the
referenced images, with `-` in the reference column (an empty `"ref"` in
JSON):

```text
-	sha256:<digest>	481
```

Such an image is addressed by its digest: `knarr inspect`, `knarr hold`,
`knarr release` and `knarr mount` all take `sha256:<digest>`. There is no
reference to `remove`; releasing its last holder makes it collectable, and
`knarr gc` reclaims it (see [gc.md](gc.md)).

## 2. Forget one reference

```sh
knarr remove "app@sha256:<digest>"
```

Removal keys on the exact reference string. On success the reference no longer
appears in `knarr ls`, and `knarr mount "app@sha256:<digest>" <target>` fails
until the image is pulled again.

`knarr remove` deletes the reference record and deletes the image binding for
the recorded `(identity digest, platform)` only when no other reference still
points at the same pair. It never deletes files under `objects/`.

If the reference is unknown, the command fails with a "no such image" error.

A plain `remove` declines while a hold pins the image, naming each holder and
the identity digest the holds sit under:

```text
image is held by cfs under sha256:<identity>; release the hold or use --force
```

Holds are keyed by that identity, so a hold taken on the image under one
reference — or by a bare digest naming its index — is seen here. Drop it with
`knarr release`, passing the digest the message named, or override with
`--force`, which clears every holder first.

## Notes

- **Exact refs.** `remove` forgets the same string that `pull` recorded; aliases
  for the same image are separate reference records.
- **Shared bindings.** Removing one alias for an image does not delete the
  binding if another surviving alias still needs it — including an alias that
  pins the index while this one pins the manifest, since both record the same
  identity.
- **No object reclamation.** Objects remain in the content-addressed store after
  removal; `knarr gc` is what reclaims content nothing wants (see
  [gc.md](gc.md)).
- **Removing a mounted image is allowed.** Like unlinking an open file:
  `remove` only forgets the index entries — the name is gone, the content
  stays — so the mount keeps serving until unmounted, but a new `knarr mount`
  of that reference fails until the image is pulled again. A later `knarr gc`
  keeps a mounted image's content through its in-use lock and reclaims it
  after unmount (see [gc.md](gc.md)).
- **Invalid reference records are skipped.** `ls` warns about and skips a
  corrupt record instead of failing; a skipped record is not a usable reference
  and re-pulling recreates it. A valid reference whose image binding is missing
  or unreadable is still listed, with size 0 and a warning — re-pulling
  restores the binding.
- **Force clears holds.** `--force` removes the image even while held, dropping
  every holder of its identity digest first.
