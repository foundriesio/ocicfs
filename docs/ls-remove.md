# Listing and forgetting local images (`knarr ls`, `knarr remove`)

`knarr ls` lists image references that have been pulled into the local store.
`knarr remove` forgets one exact reference from the auxiliary indexes. It does
not delete content objects; object reclamation is handled by `gc`.

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

The columns are the exact stored reference, the resolved root digest, and the
size of the image's composefs image object — the same size `knarr pull`
reports.

Use `--format json` for machine-readable output:

```sh
knarr ls --format json
# [
#   {"ref":"app@sha256:...","digest":"sha256:...","size":481}
# ]
```

An empty store prints no text rows, or `[]` in JSON, and exits successfully.

## 2. Forget one reference

```sh
knarr remove "app@sha256:<digest>"
```

Removal keys on the exact reference string. On success the reference no longer
appears in `knarr ls`, and `knarr mount "app@sha256:<digest>" <target>` fails
until the image is pulled again.

`knarr remove` deletes the reference record and deletes the image binding for
the resolved `(root digest, platform)` only when no other reference still points
at the same pair. It never deletes files under `objects/`.

If the reference is unknown, the command fails with a "no such image" error.

## Notes

- **Exact refs.** `remove` forgets the same string that `pull` recorded; aliases
  for the same digest are separate reference records.
- **Shared bindings.** Removing one alias for a digest does not delete the
  binding if another surviving alias still needs it.
- **No object reclamation.** Objects remain in the content-addressed store after
  removal.
- **Removing a mounted image is allowed.** The kernel keeps an active composefs
  mount alive; `remove` only forgets the index entries, so the mount keeps
  serving until unmounted — but a new `knarr mount` of that reference fails
  until the image is pulled again.
- **Invalid reference records are skipped.** `ls` warns about and skips a
  corrupt record instead of failing; a skipped record is not a usable reference
  and re-pulling recreates it. A valid reference whose image binding is missing
  or unreadable is still listed, with size 0 and a warning — re-pulling
  restores the binding.
- **Force is accepted.** `--force` is currently a no-op because there is no hold
  state to override.
