# Hands-on: building and verifying a composefs image (publisher → device)

This is a runnable walkthrough of knarr's on-disk substrate: a content-addressed
object store, per-file fs-verity, and a composefs/EROFS image that redirects
into the store. It uses only the upstream `mkcomposefs` and `composefs-info` tools so
you can build and inspect the exact artifacts knarr produces — by hand — before
reading the code.

knarr's trust model has **two sides**, and every step below is tagged with the one it
belongs to:

- **[publisher]** (CI side) builds the composefs image, measures its fs-verity, and
  **seals** that digest into the image's OCI metadata as publisher attestation.
- **[device]** rebuilds the image from the delivered content and **verifies** its
  measured fs-verity equals the sealed value.

The seal only works if both sides build a **byte-identical** image, so the recipe
(V1 format, zeroed mtimes, no inline, content-addressed objects) is fixed. This
walkthrough plays both roles on one machine.

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
the digests. (On a real device knarr also *enables* kernel fs-verity on each object;
the digest values are the same.)

Prefer one command? [`docs/storage-demo.sh`](storage-demo.sh) runs this entire
publisher → device flow against a sample tree and checks the seal. The steps below
walk through what it does.

## The format in one paragraph

A regular file's bytes are stored once, as an object named by the sha256 of its
*content*, under `objects/sha256/<aa>/<rest>`. The composefs image is an EROFS that, for
each file, records a **PAYLOAD** (a redirect to that object) and a **DIGEST** (the
file's fs-verity digest, which the kernel validates the object against). knarr keys
the store by content sha256, so PAYLOAD and DIGEST are *different* hashes of the same
file. The image holds no file content — only structure plus redirects — so one
decompressed copy of each object is shared across every image that references it.

## Publisher: build and seal

### 1. [publisher] A source tree

```sh
cd "$(mktemp -d)"
mkdir -p tree/etc tree/bin
printf 'hello from knarr' > tree/hello        # 16 bytes
printf 'k=v\n'            > tree/etc/app.conf  # 4 bytes
head -c 200 /dev/random   > tree/bin/tool      # 200 random bytes (a stand-in binary)
```

`tool` has random content, so its content and fs-verity digests differ on every run;
`hello` and `app.conf` are fixed text, so their digests reproduce exactly.

### 2. [publisher] Get the structure from mkcomposefs

Rather than hand-write a dump, build one straight from the tree and let
`composefs-info` print it. Note that `mkcomposefs` **inlines** files ≤ 64 bytes — their
content lands in the `CONTENT` column with no backing object:

```sh
mkcomposefs --use-epoch --skip-xattrs --min-version=1 --max-version=1 tree inlined.cfs
composefs-info dump inlined.cfs
```

```
/ 4096 40755 4 0 0 0 0.0 - - -
/bin/tool 200 100644 1 0 0 0 0.0 28/4c22bb..20c7 - 284c22bb..20c7   <- by-digest (>64 B)
/etc/app.conf 4 100644 1 0 0 0 0.0 - k=v\n -                        <- inlined (<=64 B)
/hello 16 100644 1 0 0 0 0.0 - hello\x20from\x20knarr -             <- inlined (<=64 B)
```

### 3. [publisher] Rewrite to a content-addressed, no-inline dump

knarr never inlines: every file is a by-digest object in the object store. This filter turns
each regular-file line into knarr's form — it stores the file under its content
sha256, then emits `PAYLOAD` = the `<aa>/<rest>` content redirect, `CONTENT` = `-`,
and `DIGEST` = the file's fs-verity digest. Directories and symlinks pass through:

```sh
mkdir -p objects/sha256
to_knarr() {
  while IFS= read -r line; do
    set -- $line                       # PATH SIZE MODE NLINK UID GID RDEV MTIME PAYLOAD CONTENT DIGEST
    case $1 in *\\*) echo "escaped PATH unsupported: $1" >&2; exit 1;; esac
    f="tree$1"
    if [ -f "$f" ] && [ ! -L "$f" ]; then
      csum=$(sha256sum "$f" | cut -d' ' -f1)                       # content digest -> PAYLOAD
      install -D -m0644 "$f" "objects/sha256/${csum:0:2}/${csum:2}"    # store the object
      fsv=$(composefs-info measure-file "$f")                      # fs-verity digest -> DIGEST
      printf '%s %s %s %s %s %s %s %s %s/%s - %s\n' \
        "$1" "$2" "$3" "$4" "$5" "$6" "$7" "$8" "${csum:0:2}" "${csum:2}" "$fsv"
    else
      printf '%s\n' "$line"
    fi
  done
}
composefs-info dump inlined.cfs | to_knarr > image.dump
cat image.dump
```

```
/bin/tool 200 100644 1 0 0 0 0.0 aa/20c2..486d - 284c22bb..20c7
/etc/app.conf 4 100644 1 0 0 0 0.0 af/33f4..274da - eed448e7..ba535
/hello 16 100644 1 0 0 0 0.0 62/531b..501b - 892bd692..4c429
```

The two formerly-inlined files are now by-digest, and you can see knarr's key point on
`tool`: `PAYLOAD` (`aa/20c2…`, the content sha256) differs from `DIGEST` (`284c22…`,
the fs-verity digest). This is the `internal/composefs` `Entry`/`WriteDump` format,
and the lines map one-to-one onto the [composefs-dump(5)](https://github.com/composefs/composefs/blob/v1.0.8/man/composefs-dump.md)
columns. (Upstream's own `--digest-store` keys objects by fs-verity instead, making
those two columns coincide — knarr keys by content so it can dedup and delta using the
digests the OCI layer TOC already carries.)

### 4. [publisher] Build the canonical image and seal it

`--from-file` builds the image straight from the dump, reading no file content;
`--min-version=1 --max-version=1` pins the stable V1 format:

```sh
mkcomposefs --min-version=1 --max-version=1 --from-file image.dump publisher.cfs
SEAL=$(composefs-info measure-file publisher.cfs)
echo "sealed fs-verity digest: $SEAL"
# e.g. 57a706da69bdedab74d77389613d40bd87ae8097d93ef576baf10a2225fdd8eb
```

In real knarr the publisher ships standard zstd:chunked OCI layers and publishes `$SEAL`
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
composefs-info objects device.cfs                              # objects this image needs
composefs-info missing-objects --basedir=objects/sha256 device.cfs # what the object store still lacks
```

`missing-objects` prints nothing here (the object store is complete); against a partial object store it
is exactly the set the device would still need to pull — the kernel of delta fetch.

## How this maps to the code

| Walkthrough | knarr code | Tests |
|---|---|---|
| fs-verity enable/measure | `internal/verity` | `internal/verity/verity_test.go` |
| content-addressed object store | `internal/objectstore` | `internal/objectstore/objectstore_test.go` |
| dump + `mkcomposefs --from-file` | `internal/composefs` (`Entry`, `WriteDump`, `WriteImage`) | `internal/composefs/*_test.go` |

The round-trip tests are the executable version of this doc — they build an image two
ways and assert byte-identity and matching fs-verity, including the no-inline case
verified against upstream-built reference images:

```sh
go test -v -run TestWriteImage ./internal/composefs/
```

## Reproducibility

The publisher seal verifies on the device only because both builds use the same
recipe: V1 format (`--min-version=1`), zeroed mtimes (`MTIME=0`, i.e. `--use-epoch`),
no inline (`CONTENT="-"`), content-addressed objects, sorted entries, and consistent
xattr handling. Byte-identical inputs give a byte-identical EROFS, hence an identical
fs-verity digest. A directory-mode publisher build would inline small files and seal a
digest the device's no-inline rebuild could never match — which is why both sides use
the `--from-file` path.

## Mounting (not covered here)

This walkthrough stops at building and verifying. Mounting an image
(`mount.composefs publisher.cfs --basedir objects/sha256 /mnt`) needs root
privilege and composefs kernel support. The `mount.composefs` helper is in the
dev image, but mounting is knarr's `mount` verb — see [mount.md](mount.md) —
and is exercised there rather than in this substrate walkthrough.
