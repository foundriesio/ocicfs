#!/usr/bin/env python3
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# Regenerates the zstd:chunked fixtures in this directory. Not run by go test.
#
# Every case is a hand-built tar layer set wrapped in an OCI layout, which
# skopeo recompresses with containers/storage's zstd:chunked writer -- the only
# writer of the format today -- so each fixture's TOC is real producer output
# while the entries come in exactly the order the case needs (a build tool
# emits parents before children and never a duplicate path). The output is
# byte-deterministic under TZ=UTC for a given skopeo; the checked-in fixtures
# came from skopeo 1.23.0. Run this script and check `git status`.
#
# Requires python3 and skopeo. Usage: ./gen.py (from any directory).

import gzip
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
MTIME = 1700000000
BIG = b"this file is bigger than sixty-four bytes so it is stored as an object.\n"  # 72 bytes
assert len(BIG) > 64

# A security.capability value (v2, cap_net_bind_service permitted+effective).
CAP = "\x01\x00\x00\x02\x00\x04\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"


def reg(name, data, **kw):
    return dict(name=name, type=tarfile.REGTYPE, data=data, **kw)


def d(name, **kw):
    return dict(name=name, type=tarfile.DIRTYPE, mode=kw.pop("mode", 0o755), **kw)


def link(name, target, **kw):
    return dict(name=name, type=tarfile.LNKTYPE, link=target, **kw)


def sym(name, target, **kw):
    return dict(name=name, type=tarfile.SYMTYPE, link=target, mode=0o777, **kw)


def wh(path):
    head, _, base = path.rpartition("/")
    return reg(f"{head}/.wh.{base}" if head else f".wh.{base}", b"")


def opq(dirname):
    return reg(f"{dirname}/.wh..wh..opq", b"")


def tar_layer(entries):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as t:
        for e in entries:
            ti = tarfile.TarInfo(e["name"])
            ti.type = e["type"]
            ti.mode = e.get("mode", 0o644)
            ti.mtime = e.get("mtime", MTIME)
            ti.uid = e.get("uid", 0)
            ti.gid = e.get("gid", 0)
            ti.devmajor = e.get("devmajor", 0)
            ti.devminor = e.get("devminor", 0)
            if "link" in e:
                ti.linkname = e["link"]
            if "xattrs" in e:
                ti.pax_headers = {"SCHILY.xattr." + k: v for k, v in e["xattrs"].items()}
            data = e.get("data", b"")
            if ti.type == tarfile.REGTYPE:
                ti.size = len(data)
                t.addfile(ti, io.BytesIO(data))
            else:
                t.addfile(ti)
    return buf.getvalue()


def write_blob(layout, data):
    dgst = hashlib.sha256(data).hexdigest()
    os.makedirs(os.path.join(layout, "blobs", "sha256"), exist_ok=True)
    with open(os.path.join(layout, "blobs", "sha256", dgst), "wb") as f:
        f.write(data)
    return "sha256:" + dgst, len(data)


def gzip_bytes(data):
    """Compresses with a zero mtime; gzip.compress only defaults to that from Python 3.14."""
    return gzip.compress(data, mtime=0)


def write_image(layout, ref, layers, compress=gzip_bytes, media="application/vnd.oci.image.layer.v1.tar+gzip"):
    """Writes one image (a list of tar layers) into layout under ref, gzip-compressed."""
    diff_ids, descs = [], []
    for raw in layers:
        diff_ids.append("sha256:" + hashlib.sha256(raw).hexdigest())
        dgst, size = write_blob(layout, compress(raw))
        descs.append({"mediaType": media, "digest": dgst, "size": size})
    config = json.dumps({"architecture": "amd64", "os": "linux", "config": {},
                         "rootfs": {"type": "layers", "diff_ids": diff_ids}}).encode()
    cdgst, csize = write_blob(layout, config)
    manifest = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                           "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cdgst, "size": csize},
                           "layers": descs}).encode()
    mdgst, msize = write_blob(layout, manifest)
    index_path = os.path.join(layout, "index.json")
    index = json.load(open(index_path)) if os.path.exists(index_path) else {"schemaVersion": 2, "manifests": []}
    index["manifests"].append({"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": mdgst, "size": msize,
                               "annotations": {"org.opencontainers.image.ref.name": ref}})
    json.dump(index, open(index_path, "w"))
    with open(os.path.join(layout, "oci-layout"), "w") as f:
        f.write('{"imageLayoutVersion":"1.0.0"}')
    return mdgst


def skopeo(src, ref, dst):
    env = dict(os.environ, TZ="UTC")
    subprocess.run(["skopeo", "copy", "--dest-compress-format", "zstd:chunked", f"oci:{src}:{ref}", f"oci:{dst}:{ref}"],
                   check=True, env=env, stdout=subprocess.DEVNULL)


# --- the cases -------------------------------------------------------------

def base_layer():
    """A small rootfs every image below starts from; also release-n's base."""
    zeros = b"A" * 3000 + b"\0" * 5000 + b"B" * 3000  # three chunks: data, zeros, data
    return [
        d("./"),
        d("usr/", mode=0o755, mtime=MTIME + 1),  # the canonical root derives its metadata from /usr
        d("usr/bin/"),
        reg("usr/bin/tool", BIG + b"tool\n", mode=0o755),
        reg("usr/bin/cap", BIG + b"cap\n", mode=0o755, xattrs={"security.capability": CAP, "user.note": "kept in the record, dropped at dump"}),
        d("usr/lib/"),
        reg("usr/lib/zeros.bin", zeros),
        d("etc/"),
        reg("etc/inline", b"tiny"),
        reg("etc/empty", b""),
        reg("etc/b63", b"6" * 63),
        reg("etc/b64", b"6" * 64),
        reg("etc/b65", b"6" * 65),
        sym("etc/link", "inline"),
        link("etc/hardlink", "usr/bin/tool"),
        reg("etc/subsecond", BIG, mtime=MTIME + 0.5),
        reg("etc/zero-mtime", BIG + b"0\n", mtime=0),
        reg("etc/pre-epoch", BIG + b"-\n", mtime=-5),
        d("dev/"),
        dict(name="dev/null", type=tarfile.CHRTYPE, mode=0o666, devmajor=1, devminor=3),
        dict(name="dev/loop0", type=tarfile.BLKTYPE, mode=0o660, devmajor=7, devminor=0),
        dict(name="dev/fifo", type=tarfile.FIFOTYPE, mode=0o644),
        d("run/"),
        reg("run/pid", b"1\n"),  # /run content is emptied by the canonical transform
    ]


CASES = {
    # One layer covering every entry kind, payload class and mtime shape.
    "basic": [base_layer()],
    # No duplicate-path case: the zstd:chunked writer refuses a layer that
    # names a path twice ("duplicates of file paths not supported"), so a
    # real TOC never carries one; the parser's tolerance of duplicates is
    # exercised on a mutated TOC in the unit tests instead.
    # A child listed before its parent flips the canonical verdict.
    "child-before-parent": [[d("./"), d("usr/"), reg("dir/x", BIG), d("dir/")]],
    # A hardlink to a hardlink, and one that names its target before the target exists.
    "hardlink-chain": [[d("./"), d("usr/"), reg("a", BIG), link("hl1", "a"), link("hl2", "hl1")]],
    "hardlink-before-target": [[d("./"), d("usr/"), link("hl", "a"), reg("a", BIG)]],
    # The link survives the whiteout of its target's name.
    "hardlink-target-whiteout": [[d("./"), d("usr/"), reg("a", BIG), link("hl", "a")], [wh("a")]],
    # Whiteout and re-add in one layer, in both orders.
    "whiteout-same-layer": [[d("./"), d("usr/"), reg("f", BIG), reg("g", BIG)],
                            [wh("f"), reg("f", BIG + b"again\n"), reg("g", BIG + b"again\n"), wh("g")]],
    # An opaque marker beside same-layer content.
    "opaque-same-layer": [[d("./"), d("usr/"), d("dir/"), reg("dir/old", BIG)],
                          [d("dir/"), opq("dir"), reg("dir/new", BIG + b"new\n")]],
    # A regular file over a lower-layer directory drops the subtree.
    "dir-replaced-by-file": [[d("./"), d("usr/"), d("dir/"), reg("dir/x", BIG)], [reg("dir", BIG + b"file\n")]],
    # Names that need normalization.
    "normalize": [[d("./"), d("usr/"), d("./a/"), reg("./a//b", BIG), reg("a/./c", BIG + b"c\n")]],
    # An extended attribute with an empty value is kept, in the tar and in the TOC.
    "empty-xattr": [[d("./"), d("usr/"), reg("usr/f", BIG, xattrs={"security.capability": ""})]],
    # No /usr and a /run that is a file: both canonical divergences.
    "noncanonical": [[d("./"), reg("run", BIG), reg("f", BIG)]],
}


def app_layer(version):
    entries = [d("app/"), reg("app/config", b"v=%d\n" % version), reg("app/data", BIG + b"data unchanged across releases\n")]
    if version == 1:
        entries.append(reg("app/old", BIG + b"old\n"))
    else:
        entries += [wh("app/old"), reg("app/new", BIG + b"new\n")]
    return entries


def main():
    os.chdir(HERE)
    for out in ("layout", "release-n", "release-n1"):
        shutil.rmtree(out, ignore_errors=True)
    with tempfile.TemporaryDirectory(dir=HERE) as tmp:
        src = os.path.join(tmp, "src")
        for name, layers in CASES.items():
            write_image(src, name, [tar_layer(l) for l in layers])
            skopeo(src, name, "layout")
        # Two releases sharing their base layer, each its own single-image
        # layout so crane can push them (crane refuses a multi-entry index).
        for ref, version, out in (("n", 1, "release-n"), ("n1", 2, "release-n1")):
            write_image(src, ref, [tar_layer(base_layer()), tar_layer(app_layer(version))])
            skopeo(src, ref, out)
        # "mixed": release-n1 with its base layer zstd:chunked (copied from
        # release-n1) and its app layer plain gzip, so one image carries a
        # TOC layer and a non-TOC layer.
        n1 = json.load(open("release-n1/index.json"))["manifests"][0]["digest"].split(":")[1]
        manifest = json.load(open(os.path.join("release-n1", "blobs", "sha256", n1)))
        config = json.load(open(os.path.join("release-n1", "blobs", "sha256", manifest["config"]["digest"].split(":")[1])))
        toc_layer = manifest["layers"][0]
        shutil.copy(os.path.join("release-n1", "blobs", "sha256", toc_layer["digest"].split(":")[1]),
                    os.path.join("layout", "blobs", "sha256", toc_layer["digest"].split(":")[1]))
        app = tar_layer(app_layer(2))
        gz_dgst, gz_size = write_blob("layout", gzip_bytes(app))
        cfg = json.dumps({"architecture": "amd64", "os": "linux", "config": {},
                          "rootfs": {"type": "layers", "diff_ids": [config["rootfs"]["diff_ids"][0], "sha256:" + hashlib.sha256(app).hexdigest()]}}).encode()
        cdgst, csize = write_blob("layout", cfg)
        man = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                          "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cdgst, "size": csize},
                          "layers": [toc_layer, {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": gz_dgst, "size": gz_size}]}).encode()
        mdgst, msize = write_blob("layout", man)
        index = json.load(open("layout/index.json"))
        index["manifests"].append({"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": mdgst, "size": msize,
                                   "annotations": {"org.opencontainers.image.ref.name": "mixed"}})
        json.dump(index, open("layout/index.json", "w"))
    total = sum(os.path.getsize(os.path.join(r, f)) for out in ("layout", "release-n", "release-n1") for r, _, fs in os.walk(out) for f in fs)
    print(f"wrote layout/ ({len(CASES) + 1} images), release-n/, release-n1/: {total} bytes", file=sys.stderr)


if __name__ == "__main__":
    main()
