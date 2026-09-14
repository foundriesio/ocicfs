# Reclaiming store space (`knarr gc`)

`knarr gc` deletes every store object that is neither **wanted** nor **in
use**, along with orphan image bindings, orphan fs-verity view entries,
staging leftovers under `tmp/`, and the empty shard directories earlier
deletions left behind across the store's fan-out trees. It is the reclamation
half of the two-layer protection model:

- **Wanted** is intent: an image reachable from a reference record (`knarr
  pull` wrote one, `knarr remove` deletes it) or pinned by a hold (`knarr
  hold`). Wanted content survives reboots and unmounts and is never
  collected.
- **In use** is fact: a mounted image holds a kernel-accounted shared lock on
  its EROFS object, and gc probes it without blocking. An in-use image
  survives even when nothing wants it anymore, together with every object it
  references — the collection follows the mounted image's redirects — and
  falls on the first gc after its unmount.

gc deletes only what is neither. Everything it deletes is re-fetchable with
`knarr pull`; holds are never touched, nor are the reference records an
agent's pulls wrote. A runtime reference — one a pull under `WithHold`
recorded, naming the image for a holder — is dropped once that holder no
longer stands on the image, since the name lived on the hold's behalf.

Some garbage is expected immediately after a pull: the pull streams each
layer and materializes every file it carries, so a file a later layer
overwrites or deletes is unreferenced by the merged image from the start —
for Debian-based images the shadowed package-database generations alone can
add up to a few MiB. The first collection reclaims them. A pull that knows
every layer's file table up front — the zstd:chunked case the planned
chunk-delta support builds on — can skip materializing shadowed files
instead of leaving them to gc.

## Usage

```sh
knarr gc                 # collect, report what was removed
knarr gc --dry-run       # report what a collection would remove, removing nothing
knarr gc --human         # sizes as KiB/MiB/... instead of exact bytes
knarr gc --list-removed  # also list each removed object's digest
knarr gc --format json
```

Text output is one line:

```text
removed 12 objects (34567890 bytes) and 1 binding; 2 in use kept
```

With `--human` the size reads `33.0 MiB`; with `--list-removed` the removed
digests precede it, one per line.

JSON output carries the same counts (`objectsRemoved`, `bytesFreed`,
`bindingsRemoved`, `refsRemoved`, `inUseSkipped`, `dryRun`), always in exact
bytes, plus a `removedObjects` array under `--list-removed`. `refsRemoved`
counts runtime references dropped because their holder no longer stood; the
text form mentions them only when there were any. The listing names deleted
store objects; orphan fs-verity view entries (possible only after external
store damage) are counted but not listed.

## Fail-safe: a broken wanted root aborts the collection

A wanted image gc cannot fully enumerate — a reference record that does not
decode, a chain that does not resolve to the recorded identity, a missing
binding, an unreadable composefs image — aborts the whole collection with
nothing deleted, naming the root and the remedy (`knarr pull` to restore it,
`knarr remove` or `knarr release` to stop wanting it). Keeping garbage one
cycle longer is always safe; deleting live data an offline device cannot
re-fetch is not. There is no partial sweep.

Part of that enumeration is trusting the binding's selection of the composefs
image, and gc guards it the way a mount does: the image must measure to the
fs-verity digest the sealing annotation of the verified identity manifest
claims, or, unsealed, to the digest the binding itself records. For a sealed
image that anchor is independent of the binding, so a repointed binding aborts
the collection. Unsealed, the binding's own record is the only authority there
is — a coherent rewrite of the whole binding is indistinguishable from a
legitimate one, for gc exactly as for mount; sealing is what closes that gap.

## Concurrency

gc takes the store lock exclusively and never blocks: while a pull, hold, or
mount is in progress it fails fast with a "store busy" error, and the caller
retries. Those three verbs hold the lock shared for their duration, so a
collection never interleaves with an image being staged, a hold being
recorded, or a mount being set up. `remove` and `release` need no lock —
racing them only makes a collection keep too much for one cycle, or, when a
`remove` lands between gc's census of the reference records and its walk of
that image, surfaces as a transient broken-root abort for the image just
removed; nothing is deleted either way, and a retry succeeds.

## In-use coverage

The in-use layer exists because wanted is intent, and intent can be stale
at the moment gc runs. The everyday case: an update agent pulls v2 of an
app, releases and removes v1, and runs gc — while the v1 container is
still draining requests, because stopping it timed out, or the agent
crashed between steps and a scheduled collection ran first, or the
container belongs to docker and its teardown is not the agent's to
sequence. Nothing wants v1 anymore, and a live mount does not hold its
data files open — the overlay reads store objects lazily, on first
access — so collecting them would hand the still-running container an
I/O error on the next file it touches. The shared lock turns that from a
sequencing obligation on every caller into a property of the store: gc
sees the v1 image locked, keeps it and everything it references, and
reclaims it all on the first collection after the unmount.

All knarr mounts are covered for their lifetime: on both mount paths the
locked file description is the one the loop device attaches, so the lock
lives exactly as long as the mount, surviving the mounting process. In-use
protection still ends at unmount — and at reboot, which ends every mount and
its lock. **Hold any image that must stay protected beyond that** — holds,
not the in-use probe, are the durable protection, and the lock is advisory
everywhere: it is accounting, not integrity (fs-verity is integrity).

`mount` deliberately does not take that hold itself. A hold is durable caller
intent and a mount is ephemeral kernel state, and pairing them leaks: holds
survive the reboot or crash that silently ends the mount, leaving gc
over-retaining and `remove` declining for a mount that no longer exists, with
no release path — `unmount` knows only a target path, not an image, and
concurrent mounts of one image would need reference counting on top.
Protection with exactly a mount's lifetime can only come from kernel-accounted
state that dies with it, which is what the flock is. A caller that wants an
image protected beyond that composes it explicitly — `hold` before mounting,
`release` after unmounting — and then owns the durable bookkeeping, the same
pairing the cfs-snapshotter drives off containerd's own snapshot records.
