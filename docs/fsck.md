# Checking the store (`ocicfs fsck`)

`ocicfs fsck` reads stored content back and compares it with its digests. It
uses only local state: no registry, no network.

| You want to | Run |
| --- | --- |
| check everything in the store | `ocicfs fsck` |
| check one or more images | `ocicfs fsck REF...` |
| read the result from a program | add `--format json` and use the exit status |
| repair a damaged store | `ocicfs fsck --fix`, then `ocicfs pull` each image it marks ([Repairing](#repairing)) |
| repair one image | `ocicfs fsck --fix`, then `ocicfs pull REF` if the image is marked `PULL AGAIN` |

## What is checked

With references, fsck checks the images they name, for the host platform:
the index the reference goes through, if it has one, the manifest and
config, the composefs image, and every file object the composefs image
refers to. This is what the check of the whole store reads for that image.
A reference that names no local image is an error.

With no reference, fsck checks every object in the store and every image
in it, on every platform it is stored for: the images a reference or a hold
keeps, and also an image that still has its binding though nothing keeps it
any more, since a later pull of that image answers that it is already
there. Objects no image needs are checked too: `ocicfs gc` has not removed
them yet, and a later pull reuses any object that is already present.

Every object has two names, one derived from the sha256 of its content and
one from its fs-verity digest. fsck reads each object once, computes both
digests, and compares them with the names. This works the same whether the
store runs with fs-verity or without it. A file of 64 bytes or less is kept
inside the composefs image and is not an object of its own.

| Problem | Meaning |
| --- | --- |
| `damaged` | the bytes match none of the object's names, or reading them back failed with an I/O or corruption error. A file without fs-verity in a store that runs with it is damaged in the same way when its bytes match none of its names |
| `misnamed` | the bytes are intact, but one of the two names is absent or belongs to different content |
| `missing` | an image needs the object and the store does not have it |

An image is also not intact when its metadata does not resolve, it has no
composefs image for the platform, or the composefs image is not the one
recorded for it. The report gives that as the image's `error`.

fsck reports what it could read back. It is not a scan of the storage
medium: a block that fails later, or one the filesystem does not use, is
outside what it sees.

## Reading the result

| Exit status | Meaning |
| --- | --- |
| 0 | everything checked is intact |
| 3 | damage was found; the report says where. Also after `--fix` |
| 1 | the check could not run or could not finish |
| 2 | wrong usage |

Status 1 covers any failure that is not evidence about an object's bytes: a
reference that names no local image, a permission error, an interrupted
run, or an intact object without fs-verity in a store that runs with it,
which means the store was written without fs-verity. fsck
stops at the first such failure and prints the error and no report: a
report made from a part of the checks would call intact what was never
read. Nothing is known about the store until a run finishes.

Text output has one line per image, one indented line per failed object,
and a summary:

```console
$ ocicfs fsck
sha256:5f0c… linux/arm64 intact registry.example.com/app@sha256:5f0c…
sha256:9a41… linux/arm64 NOT INTACT registry.example.com/db@sha256:9a41…
  damaged  sha256:77be… fsverity sha256:c2d0… content has digest sha256:1b3e… and fs-verity digest sha256:08aa…, which match no name of the object
2 images checked, 1 not intact; 0 other objects failed
to repair: make a source for these images reachable, run `ocicfs fsck --fix`, then pull each image it marks
```

`--format json` prints one object:

```json
{
  "intact": false,
  "images": [
    {
      "digest": "sha256:9a41…",
      "platform": "linux/arm64",
      "refs": ["registry.example.com/db@sha256:9a41…"],
      "intact": false,
      "error": "",
      "objects": [
        {
          "digest": "sha256:77be…",
          "fsverity": "sha256:c2d0…",
          "problem": "damaged",
          "detail": "content has digest sha256:1b3e… and fs-verity digest sha256:08aa…, which match no name of the object"
        }
      ]
    }
  ]
}
```

- `images[].digest` is the image's manifest digest, and `refs` the
  references that name it. An image kept only by a hold has no reference
  of its own; `refs` then lists the runtime references naming it, if any.
  An image nothing keeps has an empty `refs`: `ocicfs gc` removes it, and
  after `--fix` it needs no pull unless it is wanted again.
- `objects[]` lists only the objects that failed. `digest` and `fsverity`
  are the names the object has, or for a missing object the name it is
  needed under; either can be absent.
- `unreferenced` appears in a store-wide check when objects no image needs
  failed, with the same fields as `objects[]`.

## Repairing

`ocicfs fsck --fix` checks the whole store and then repairs it. It takes no
reference, for two reasons. A damaged object is usually shared between
images, so every image that needs it has to be restored. And a pull stores
every file of every layer, including a file that a later layer of the image
hides, which is in no image's list of objects. When such a hidden object is
damaged and still in the store, the pull meets it: without fs-verity the
pull compares the stored bytes with the new ones and fails, and with
fs-verity it compares only the fs-verity digest recorded for the stored
file, so the pull succeeds and the damaged object stays. So every damaged
object has to go first.

1. Make a source for the affected images reachable: the registry, or the
   archive an image was loaded from. `mkcomposefs` must be installed.
2. Run `ocicfs fsck --fix`. It exits 3 and marks each image to pull.
3. Pull every marked image: `ocicfs pull REF`, `ocicfs pull --delta REF`,
   or `ocicfs pull --from-tar ARCHIVE REF`. Each image needs its own pull
   even when it downloads nothing, because the pull reads the manifest and
   config from the source. The first pull of a shared file downloads it; the
   others only rebuild their composefs image. With `--delta`, the pull's
   log counts as `missing` every file it reads from the source, which
   includes the small files kept inside the composefs image, so the count
   is higher than the number of lost objects.
4. Run `ocicfs fsck` again. It exits 0 when the store is restored.

What the repair does with each finding:

| Finding | Repair |
| --- | --- |
| `misnamed` | the correct name is linked, then the wrong one removed. Nothing is deleted or downloaded, and the image keeps working. If another object holds the correct name, nothing is changed |
| `damaged` | the object is deleted |
| `missing` | nothing to delete |
| an image left without its manifest, its config, its composefs image, or a file object | its binding is removed, whether or not a reference names the image, so the next pull restores it |
| an image whose reference goes through an index that is missing | the binding stays, as the image itself is complete, and it still mounts. `ocicfs gc` refuses to run until that reference is pulled: the pull stores the index again and downloads nothing else |

Reference records and holds are never changed.

**What it costs.** Before the repair, an affected image still mounts and
only the damaged file fails. After it, the image is wanted but not pulled:
`ocicfs mount` and `ocicfs inspect` report it as not found, and `ocicfs gc`
refuses to run and names `ocicfs pull` as the remedy. This lasts until the image is pulled again. An image kept only by a
hold is not listed by `ocicfs ls` in that state; the fsck report is the list
to work from.

**What it leaves, reported with `"fixed": false`:**

- A damaged file that a mounted image uses, marked `"in_use": true`, with
  the image's binding. Unmount the image and run `--fix` again, or add
  `--force`. With `--force` the file is deleted under the mount: it is
  absent for new opens until the pull restores it, and files already open
  keep the old content. When a mounted image is itself too damaged to list
  its files, every damaged file counts as used by it, and `--force` does
  not change that. Under cfs-snapshotter an image stays mounted for as long
  as Docker has it: see [With cfs-snapshotter](#with-cfs-snapshotter).
- A damaged composefs image that is mounted, with its binding. Unmount its
  users and run `--fix` again.
- A misnamed object whose correct name another object holds, for example
  two objects that carry each other's name. A repair never takes a name
  from an object or removes an object's last name, so both are left as
  they are, and their content stays readable under the names they have.
  ocicfs does not decide between them. Once the object holding the name is
  itself removed, by a repair when it is damaged or by `ocicfs gc` when no
  image needs it, running `--fix` again completes the repair.
- An object that changed between the check and the repair, with the detail
  "changed during the run". Run `--fix` again.
- An object that cannot be opened at all. Nothing can tell whether a mount
  relies on it, so ocicfs does not remove it, and it keeps `ocicfs gc` from
  running. Check the storage; once nothing mounts the object, remove its
  names under `objects/sha256/` and `objects/fsverity-sha256-12/` by hand
  and run `--fix` again.

A damaged object that a hold names directly, outside any image, is deleted
like any other. No pull can bring it back; `ocicfs release` the hold.

The repair fails with status 1 and changes nothing when `mkcomposefs` is
not installed, when another command holds the store lock, in practice a
running pull ("store busy"; retry; a mounted image does not make the store
busy), or when the second check of a failed object stops on an error that
is not evidence about its bytes. It is not atomic after its first change:
if it is interrupted, run it again. Its deletions are made durable at the
end of the run, so after a power loss during a repair the store can be as
it was before the command, with the same damage to repair.

With `--fix`, the JSON report describes the store as it was checked, plus:

- `objects[].fixed`: true when a damaged object was deleted or a misnamed
  one had its names repaired; false comes with the reason in `detail`.
- `objects[].in_use`: true when the object was left because of a mount.
- `images[].binding_removed`: true when the image must be pulled again.

`intact` stays false and the exit status 3 after a repair, also when
nothing is left to pull; the next `ocicfs fsck` gives the new state.

### With cfs-snapshotter

This is what a repair of the store under Docker with cfs-snapshotter
v0.5.0 showed, for a damaged file of an image Docker has:

- The snapshotter keeps the composefs image of every image Docker has
  mounted, with or without a container. A damaged file of such an image
  therefore always counts as in use, and `--fix` leaves it. Stopping the
  containers does not change that. The repair needs `ocicfs fsck --fix
  --force`.
- `--force` deletes the damaged file under that mount and removes the
  image's binding. Docker still lists the image and its containers still
  start; the file is absent in them ("no such file") until the image is
  restored.
- Docker does not pull again an image it already has: `docker pull` answers
  that the image is up to date and fetches nothing. Restore the image with
  `ocicfs pull REF` against the same store. With the snapshotter in its
  default mode, `docker image rm` followed by `docker pull` restores it
  too.
- `ocicfs pull REF` records a reference of its own, so afterwards `ocicfs
  remove REF`, not `docker image rm`, frees the image.
- A container that opened the file while it was damaged keeps getting an
  I/O error for it after the restore, also after `docker restart`. Create
  a new container. A container that did not open the file until the
  restore reads it correctly.

## Limits

- Before reading an object, fsck asks the kernel to drop its cached pages
  so that the read reaches the storage. This is best effort: pages a
  running container has mapped, and cached fs-verity tree blocks, can still
  be served from memory.
- When fsck stops on an error that is not evidence of damage, it exits 1
  and the error names the object's path. Retry first. If the same object
  fails again, no ocicfs command removes it: check the storage, and once
  nothing mounts the object remove its names under `objects/sha256/` and
  `objects/fsverity-sha256-12/` by hand, then run `ocicfs fsck` again. The
  image that needed the object is then reported with a missing object, and
  `--fix` followed by a pull restores it.
- One such error is `invalid argument` on opening an object in a store that
  runs with fs-verity. The kernel returns it when the object's fs-verity
  descriptor is not valid, and logs the reason (`dmesg` shows a line
  starting with `fs-verity`). fsck does not count it as damage, because the
  same error has other causes, so it stops as described above.

## Running alongside other commands

fsck holds the store lock shared for its whole run, as a pull does. Pulls,
holds, and mounts proceed alongside it; `ocicfs gc` reports the store as
busy until the check ends (see [gc.md](gc.md)). A repair takes the lock
exclusively once the check is done, as gc does for its deletions. An object a pull commits
while the check runs is not reported.

A store-wide check reads every byte in the store once, so its run time is
that of reading the store from its medium.
