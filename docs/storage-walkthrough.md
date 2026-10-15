# Hands-on: building and verifying a composefs image (publisher → device)

This is a runnable walkthrough of ocicfs's on-disk substrate: a content-addressed
object store, per-file fs-verity, and a composefs/EROFS image that redirects
into the store. It uses only the upstream `mkcomposefs` and `composefs-info` tools so
you can build and inspect the exact artifacts ocicfs produces — by hand — before
reading the code.

ocicfs's trust model has **two sides**, and every step below is tagged with the one it
belongs to:

- **[publisher]** (CI side) builds the composefs image, measures its fs-verity, and
  **seals** that digest into the image's OCI metadata as publisher attestation.
- **[device]** rebuilds the image from the delivered content and **verifies** its
  measured fs-verity equals the sealed value.

The seal only works if both sides build a **byte-identical** image, so the recipe
(V1 format, per-file mtimes carried from the layer tar, files ≤ 64 bytes inlined,
fs-verity-keyed redirects for larger files) is fixed. This walkthrough plays both
roles on one machine.

For *why* the format looks like this, see [why.md](why.md) and
[adr/0001-high-level-architecture.md](adr/0001-high-level-architecture.md).

## Prerequisites

From the repo root, open a shell in the dev container (it has `mkcomposefs` and
`composefs-info`, and matches CI):

```sh
./devshell.sh
```

No elevated privilege is needed: `composefs-info measure-file` computes a file's
fs-verity digest in userspace, so you do not need an fs-verity-capable mount to see
the digests. (On a real device ocicfs also *enables* kernel fs-verity on each object;
the digest values are the same.)

Prefer one command? [`docs/storage-demo.sh`](storage-demo.sh) runs this entire
publisher → device flow against a sample tree and checks the seal. The steps below
walk through what it does.

## The format in one paragraph

A regular file **larger than 64 bytes** has its bytes stored once, as an object
named by the sha256 of its *content*, under `objects/sha256/<aa>/<rest>`, with a
second hardlink name under `objects/fsverity-sha256-12/<vv>/<rest>` keyed by the
file's fs-verity digest (the view). The composefs image is an EROFS that, for each
such file, records a **PAYLOAD** (a redirect resolved through the view) and a
**DIGEST** (the same fs-verity digest, which the kernel validates the object
against) — both columns derive from the fs-verity digest, matching upstream
composefs. The image holds no content for those files — only structure plus
redirects — so one decompressed copy of each object is shared across every image
that references it. A file of **64 bytes or fewer** is instead inlined straight
into the EROFS metadata (its bytes in the `CONTENT` column, no object), exactly as
`mkcomposefs` does.

## Publisher: build and seal

### 1. [publisher] A source tree

```sh
cd "$(mktemp -d)"
mkdir -p tree/etc tree/bin
printf 'hello from ocicfs' > tree/hello        # 16 bytes
printf 'k=v\n'            > tree/etc/app.conf  # 4 bytes
head -c 200 /dev/random   > tree/bin/tool      # 200 random bytes (a stand-in binary)
```

`tool` has random content, so its content and fs-verity digests differ on every run;
`hello` and `app.conf` are fixed text, so their digests reproduce exactly.

### 2. [publisher] Get the structure from mkcomposefs

Rather than hand-write a dump, build one straight from the tree and let
`composefs-info` print it. Note that `mkcomposefs` **inlines** files ≤ 64 bytes — their
content lands in the `CONTENT` column with no backing object. ocicfs inlines at the
same threshold, so these small-file lines are already in ocicfs's form:

```sh
mkcomposefs --use-epoch --skip-xattrs --min-version=1 --max-version=1 tree inlined.cfs
composefs-info dump inlined.cfs
```

```
/ 4096 40755 4 0 0 0 0.0 - - -
/bin/tool 200 100644 1 0 0 0 0.0 28/4c22bb..20c7 - 284c22bb..20c7   <- by-digest (>64 B)
/etc/app.conf 4 100644 1 0 0 0 0.0 - k=v\n -                        <- inlined (<=64 B)
/hello 16 100644 1 0 0 0 0.0 - hello\x20from\x20ocicfs -             <- inlined (<=64 B)
```

### 3. [publisher] Externalize the files over 64 bytes

ocicfs keeps files ≤ 64 bytes inlined (just as `mkcomposefs` already emitted them)
and turns only **larger** regular files into by-digest objects. This filter, for
each regular file over 64 bytes, stores it under its content sha256, hardlinks it
into the fs-verity view, then emits `PAYLOAD` = the `<vv>/<rest>` fs-verity
redirect, `CONTENT` = `-`, and `DIGEST` = the same fs-verity digest. Inlined
small files, directories, and symlinks pass through unchanged:

```sh
mkdir -p objects/sha256 objects/fsverity-sha256-12
to_ocicfs() {
  while IFS= read -r line; do
    set -- $line                       # PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST
    case $1 in *\\*) echo "escaped PATH unsupported: $1" >&2; exit 1;; esac
    f="tree$1"
    # Only externalize regular files over 64 bytes; smaller ones stay inlined ($2 = SIZE).
    if [ -f "$f" ] && [ ! -L "$f" ] && [ "$2" -gt 64 ]; then
      csum=$(sha256sum "$f" | cut -d' ' -f1)                       # content digest -> CAS name
      install -D -m0644 "$f" "objects/sha256/${csum:0:2}/${csum:2}"    # store the object
      fsv=$(composefs-info measure-file "$f")                      # fs-verity digest -> PAYLOAD + DIGEST
      mkdir -p "objects/fsverity-sha256-12/${fsv:0:2}"
      ln -f "objects/sha256/${csum:0:2}/${csum:2}" "objects/fsverity-sha256-12/${fsv:0:2}/${fsv:2}"
      printf '%s %s %s %s %s %s %s %s %s/%s - %s\n' \
        "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "${fsv:0:2}" "${fsv:2}" "$fsv"
    else
      printf '%s\n' "$line"
    fi
  done
}
composefs-info dump inlined.cfs | to_ocicfs > image.dump
cat image.dump
```

```
/bin/tool 200 100644 1 0 0 0 0.0 28/4c22bb..20c7 - 284c22bb..20c7   <- externalized (>64 B)
/etc/app.conf 4 100644 1 0 0 0 0.0 - k=v\n -                        <- still inlined (<=64 B)
/hello 16 100644 1 0 0 0 0.0 - hello\x20from\x20ocicfs -             <- still inlined (<=64 B)
```

Only `tool` (over 64 bytes) becomes by-digest; the two small files keep their inline
`CONTENT`. For the externalized file `PAYLOAD` and `DIGEST` both
derive from its fs-verity digest — the redirect resolves through the
fs-verity view, exactly like upstream's `--digest-store` layout. This is the
`internal/composefs` `Entry`/`WriteDump` format, and the lines map one-to-one onto
the [composefs-dump(5)](https://github.com/composefs/composefs/blob/v1.0.8/man/composefs-dump.md)
columns. (ocicfs still *stores* each object under its content sha256 — the name the
OCI layer TOC already carries, which is what dedup and delta fetch key on — and the
view hardlink gives the same inode its fs-verity name for the mount data plane.)

### 4. [publisher] Build the canonical image and seal it

`--from-file` builds the image straight from the dump, reading no file content;
`--min-version=1 --max-version=1` pins the stable V1 format:

```sh
mkcomposefs --min-version=1 --max-version=1 --from-file image.dump publisher.cfs
SEAL=$(composefs-info measure-file publisher.cfs)
echo "sealed fs-verity digest: $SEAL"
# e.g. 57a706da69bdedab74d77389613d40bd87ae8097d93ef576baf10a2225fdd8eb
```

In real ocicfs the publisher ships standard zstd:chunked OCI layers and publishes `$SEAL`
in the image's OCI metadata; `image.dump` + `objects/` here stand in for the content the
device will reconstruct.

## Device: rebuild and verify

### 5. [device] Rebuild from the delivered content

The device has fetched the objects (delta — only those missing from its object store) and
synthesises the same dump from the layer TOCs. Here it reuses `image.dump` + `objects/`.
It runs the **same** `--from-file` recipe:

```sh
mkcomposefs --min-version=1 --max-version=1 --from-file image.dump device.cfs
```

### 6. [device] Verify against the seal

```sh
GOT=$(composefs-info measure-file device.cfs)
[ "$GOT" = "$SEAL" ] && echo "VERIFY OK: matches the sealed digest" || echo "TAMPERED"
```

The device's independently-rebuilt image measures the *same* fs-verity digest the
publisher sealed — because both used the identical recipe. That equality is the
integrity guarantee.

### 7. [device] The delta view

```sh
composefs-info objects device.cfs                              # objects this image needs (fs-verity names)
composefs-info missing-objects --basedir=objects/fsverity-sha256-12 device.cfs # what the view still lacks
```

`missing-objects` prints nothing here (the object store is complete); against a partial object store it
is exactly the set the device would still need to pull — the kernel of delta fetch.

## How this maps to the code

| Walkthrough | ocicfs code | Tests |
|---|---|---|
| fs-verity enable/measure | `internal/verity` | `internal/verity/verity_test.go` |
| content-addressed object store | `internal/objectstore` | `internal/objectstore/objectstore_test.go` |
| dump + `mkcomposefs --from-file` | `internal/composefs` (`Entry`, `WriteDump`, `WriteImage`) | `internal/composefs/*_test.go` |

The round-trip tests are the executable version of this doc — they build an image two
ways and assert byte-identity and matching fs-verity, with small files inlined and
larger ones by-digest, verified against upstream-built reference images:

```sh
go test -v -run TestWriteImage ./internal/composefs/
```

## Reproducibility

The publisher seal verifies on the device only because both builds use the same
recipe: V1 format (`--min-version=1`), per-file mtimes (here pinned with
`--use-epoch` so this hand-run reproduces; real ocicfs carries each file's mtime
from the layer tar), files ≤ 64 bytes inlined (`CONTENT` = the file bytes) and
larger files by digest (fs-verity-keyed redirects), sorted entries, and consistent
xattr handling. Both sides feed the **same** `--from-file` dump, so every column —
which files are inlined, their mtimes, the redirects — is identical; byte-identical
inputs give a byte-identical EROFS, hence an identical fs-verity digest.

## Mounting (not covered here)

This walkthrough stops at building and verifying. Mounting an image — the
EROFS metadata over a loop device plus an overlay with
`objects/fsverity-sha256-12` as its data-only lower — needs root privilege
and kernel support (EROFS, fs-verity, overlayfs). Mounting is ocicfs's `mount`
verb — see [mount.md](mount.md) — and is exercised there rather than in this
substrate walkthrough.
