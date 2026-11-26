# Checking the store (`ocicfs fsck`)

`ocicfs fsck` reads stored content back and compares it with its digests. It
uses only local state: no registry, no network.

| You want to | Run |
| --- | --- |
| check everything in the store | `ocicfs fsck` |
| check one or more images | `ocicfs fsck REF...` |
| read the result from a program | add `--format json` and use the exit status |

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
| 3 | damage was found; the report says where |
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
- `objects[]` lists only the objects that failed. `digest` and `fsverity`
  are the names the object has, or for a missing object the name it is
  needed under; either can be absent.
- `unreferenced` appears in a store-wide check when objects no image needs
  failed, with the same fields as `objects[]`.

## Running alongside other commands

fsck holds the store lock shared for its whole run, as a pull does. Pulls,
holds, and mounts proceed alongside it; `ocicfs gc` reports the store as
busy until the check ends (see [gc.md](gc.md)). An object a pull commits
while the check runs is not reported.

A store-wide check reads every byte in the store once, so its run time is
that of reading the store from its medium.
