#!/usr/bin/env bash
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0
#
# ocicfs end-to-end test: exercise the image lifecycle and sealing against a real
# image and a real registry.
#
# Phase 0 builds a purpose-built image offline with umoci -- object, inline,
# and empty files, a symlink, /run content the canonical transform drops, and
# a second layer with a whiteout and an override, deliberately hardlink-free
# -- pushes it to the local registry, seals it as a plain manifest target
# (the pushing seal reads from the local layout via --from-dir, after a
# dry-run parity check against the registry source; the busybox seal leg
# below covers the registry-sourced push), runs seal-crosscheck.sh on it
# expecting the exact four-way digest match, and drives the sealed image
# through the full verb lifecycle.
#
# The rest exercises a real image: the full lifecycle -- pull -> ls ->
# inspect -> mount -> unmount, then mount --metadata-only with the overlay a
# writable consumer assembles over it (the objects/fsverity-sha256-12 view as
# lowerdir, verity=require), then hold (a plain remove must decline naming
# the holder) -> release -> remove -- five times: for an image
# sideloaded with pull --from-dir, for the same image sideloaded from a tar
# of that layout with pull --from-tar (the shape docker save writes, pulled
# with no reference, by the name the archive carries), for its host-platform
# image sideloaded the same way from a tar of a single-platform layout (the
# shape docker save --platform writes), for the same image
# pulled digest-pinned from a loopback
# HTTP registry (zot), and for the same image pulled and addressed by tag
# (checked to resolve to the same pinned digest); then ocicfs copy writes
# that tag to an archive, whose index.json entries and host-only blobs are
# checked, and the image goes through the lifecycle twice more from it with
# pull --from-tar, unpinned and pinned to the platform manifest,
# so the currently implemented functionality can be checked by hand in one
# command. With E2E_ARCHIVE set, a real archive (docker save, or
# docker buildx -o type=oci) goes through the same lifecycle too. A final
# differential leg diffs the mounted rootfs against the same image flattened
# by crane export, so the layer merge is checked against an independent
# implementation on a real image. A sealing leg then runs
# `ocicfs seal` against the registry for the host platform of the multi-arch
# image -- dry-run parity, the annotation check on the raw pushed manifest,
# and the sealed image's own full lifecycle, whose pull verifies the seal
# device-side. A last leg builds the required and off build modes beside
# the default auto one and checks the fs-verity state each reports and runs
# with on tmpfs and on the verity loopback, that a required build reports a
# filesystem without fs-verity unsupported and refuses it and a store
# written without fs-verity, and that an off build reads objects with it. The registry legs run against a local zot
# instance -- a real OCI registry that enforces the distribution spec on
# manifest PUTs, unlike an in-process test registry. It needs a privileged,
# fs-verity-capable environment with mkcomposefs on PATH -- i.e. the dev
# container. Either open the dev container and run the target
# inside it:
#
#   ./devshell.sh                # opens a shell in the dev container; then: make test-e2e
#
# or run it one-shot against a container built from the Containerfile:
#
#   docker run --rm --privileged -v "$PWD":/work ocicfs-dev make test-e2e
#
# Env:
#   E2E_IMAGE     registry image to pull (default docker.io/library/busybox:latest)
#   E2E_LAYOUT    OCI layout directory; cached across runs, so a repeat run needs no
#                 network (pre-populate it to run fully offline; the registry leg is
#                 loopback-local and needs no network either)
#   E2E_REG_PORT  loopback port for the e2e run's own zot instance
#                 (default 5001; the dev container's persistent registry
#                 owns 5000)
#   E2E_ARCHIVE   a docker save or docker buildx -o type=oci archive
#                 (uncompressed tar, one image saved by name) to run
#                 through the lifecycle with pull --from-tar and no
#                 reference; skipped when unset
set -euo pipefail

IMAGE="${E2E_IMAGE:-docker.io/library/busybox:latest}"
LAYOUT="${E2E_LAYOUT:-/tmp/ocicfs-e2e-layout}"
ARCHIVE="${E2E_ARCHIVE:-}"
STORE=""
MNT=""
OVL=""
REGPID=""
REGDIR=""
IMGDIR=""
EXPORTDIR=""
GCOVL=""
DSTORE=""
TARBALL=""
TARINDEX=""
COPYTAR=""
VDIR=""
SECTION="setup"
NSECTIONS=0
NSKIPPED=0
FAILMSG=""
TITLES=()
STATUS=()

cleanup() {
	local rc=$?
	set +e
	if [ -n "$OVL" ]; then
		bin/ocicfs --root "$STORE" unmount "$OVL/merged" 2>/dev/null
		bin/ocicfs --root "$STORE" unmount "$OVL/meta" 2>/dev/null
		rm -rf "$OVL"
	fi
	if [ -n "$GCOVL" ]; then
		bin/ocicfs --root "$STORE" unmount "$GCOVL/merged" 2>/dev/null
		rm -rf "$GCOVL"
	fi
	[ -n "$MNT" ] && bin/ocicfs --root "$STORE" unmount "$MNT" 2>/dev/null
	[ -n "$REGPID" ] && kill "$REGPID" 2>/dev/null
	[ -n "$REGDIR" ] && rm -rf "$REGDIR"
	[ -n "$IMGDIR" ] && rm -rf "$IMGDIR"
	[ -n "$STORE" ] && rm -rf "$STORE"
	[ -n "$MNT" ] && rm -rf "$MNT"
	[ -n "$EXPORTDIR" ] && rm -rf "$EXPORTDIR"
	[ -n "$DSTORE" ] && rm -rf "$DSTORE"
	[ -n "$TARBALL" ] && rm -f "$TARBALL"
	[ -n "$TARINDEX" ] && rm -rf "$TARINDEX"
	[ -n "$COPYTAR" ] && rm -f "$COPYTAR"
	if [ -n "$VDIR" ]; then
		if mountpoint -q "$VDIR/tmpfs" && ! umount "$VDIR/tmpfs"; then
			rc=1
			FAILMSG="cannot unmount $VDIR/tmpfs; left $VDIR in place"
		else
			rm -rf "$VDIR"
		fi
	fi
	# The summary and verdict are the last lines of the run, after any cleanup
	# output. Sections after a failure never ran, so they are not listed.
	if [ "$NSECTIONS" -gt 0 ]; then
		[ "$rc" -eq 0 ] || STATUS[NSECTIONS - 1]=FAIL
		printf '\nE2E summary (%ss):\n' "$SECONDS"
		for i in "${!TITLES[@]}"; do
			printf '  %-4s %2d. %s\n' "${STATUS[i]}" $((i + 1)) "${TITLES[i]}"
		done
	fi
	if [ "$rc" -eq 0 ]; then
		echo "E2E OK: $((NSECTIONS - NSKIPPED)) passed, $NSKIPPED skipped"
	else
		echo "E2E FAIL in $SECTION: ${FAILMSG:-exit $rc}" >&2
	fi
	exit "$rc"
}
trap cleanup EXIT

STORE="$(mktemp -d /var/lib/ocicfs/e2e-XXXXXX)"
MNT="$(mktemp -d)"
KN=(bin/ocicfs --root "$STORE")

# section starts the next numbered section, step announces a step within it,
# and skip marks the current section as not run; the exit trap lists them all
# in its summary. fail records the message the exit trap reports, together with
# the section it failed in.
section() {
	NSECTIONS=$((NSECTIONS + 1))
	SECTION="$NSECTIONS. $*"
	TITLES+=("$*")
	STATUS+=(ok)
	printf '\n=== %s ===\n' "$SECTION"
}
step() { echo "  -- $*"; }
skip() {
	NSKIPPED=$((NSKIPPED + 1))
	STATUS[NSECTIONS - 1]=skip
	TITLES[NSECTIONS - 1]+=": $*"
	echo "  -- skipped: $*"
}
fail() {
	FAILMSG="$*"
	exit 1
}

# host_child prints the digest of the manifest an image index offers for the
# host platform -- the identity ocicfs keys the image by. $1 is the index JSON.
host_child() {
	jq -r --arg arch "$(go env GOARCH)" \
		'[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch)][0].digest' \
		<<<"$1"
}

# run_lifecycle drives every implemented verb for one image: $1 is the
# reference the store ends up holding (digest-pinned or a tag), $2 the image
# identity it must resolve to (the platform manifest), $3 the digest the
# reference itself lands on (the same as $2 unless the reference names an
# index), and the remaining arguments are the pull invocation that puts it
# there.
run_lifecycle() {
	local ref="$1" digest="$2" pulled="$3"
	shift 3

	step "$*"
	"${KN[@]}" "$@"

	step "ls: expect the pulled image"
	local listed
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed"
	printf '%s\n' "$listed" | grep -qF "$ref" || fail "ls did not list $ref after pull"

	step "inspect, text and json: expect the pulled digest"
	"${KN[@]}" inspect "$ref"
	local inspected
	inspected="$("${KN[@]}" inspect --format json "$ref")"
	[ "$(jq -r .root <<<"$inspected")" = "$digest" ] || fail "inspect root is not the image identity"

	step "check the ref record: expect it keyed by identity, with the source digest kept"
	# One image is in the store at this point, so the single ref record under
	# refs/ is this leg's. The on-disk layout is a documented contract.
	local record
	record="$(cat "$STORE"/refs/sha256/*/*.json)"
	printf '%s\n' "$record"
	[ "$(jq -r .identity.digest <<<"$record")" = "$digest" ] || fail "ref record identity is not the image identity"
	[ "$(jq -r .resolved.digest <<<"$record")" = "$pulled" ] || fail "ref record resolved is not the pulled digest"
	jq -e '.composefs.digest | startswith("sha256:")' <<<"$inspected" >/dev/null || fail "inspect reports no composefs digest"
	jq -e '.fsverity != ""' <<<"$inspected" >/dev/null || fail "inspect reports no fsverity digest"
	jq -e '.rootfs_size > 0' <<<"$inspected" >/dev/null || fail "inspect reports no merged rootfs size"

	step "inspect --layers, table and json: expect the layer identities"
	"${KN[@]}" inspect --layers "$ref"
	local layered
	layered="$("${KN[@]}" inspect --layers --format json "$ref")"
	[ "$(jq -r '.layers | length' <<<"$layered")" -ge 1 ] || fail "inspect --layers reports no layers"
	jq -e '[.layers[].diffID | startswith("sha256:")] | all' <<<"$layered" >/dev/null || fail "a layer diffID is malformed"
	[ "$(jq -r '.layers[0].chainID' <<<"$layered")" = "$(jq -r '.diff_ids[0]' <<<"$layered")" ] \
		|| fail "first layer chainID is not the first diff_id"
	jq -e '[.layers[].size > 0] | all' <<<"$layered" >/dev/null || fail "a layer reports no archive size"

	step "mount"
	"${KN[@]}" mount "$ref" "$MNT"
	step "check the mounted rootfs: expect it non-empty and read-only"
	ls -la "$MNT"
	[ -n "$(ls -A "$MNT")" ] || fail "mounted rootfs is empty"
	if touch "$MNT/.e2e-rw-probe" 2>/dev/null; then fail "mounted rootfs is writable"; fi

	step "unmount: expect the mountpoint emptied"
	"${KN[@]}" unmount "$MNT"
	[ -z "$(ls -A "$MNT")" ] || fail "mountpoint not empty after unmount"

	step "mount --metadata-only and assemble a writable overlay over it"
	# The upper and work directories must not live on an overlayfs, so keep the
	# whole leg beside the store on the verity-capable filesystem.
	OVL="$(mktemp -d /var/lib/ocicfs/e2e-ovl-XXXXXX)"
	mkdir "$OVL/meta" "$OVL/merged" "$OVL/upper" "$OVL/work"
	"${KN[@]}" mount --metadata-only "$ref" "$OVL/meta"
	mount -t overlay overlay -o \
		"lowerdir=$OVL/meta::$STORE/objects/fsverity-sha256-12,upperdir=$OVL/upper,workdir=$OVL/work,redirect_dir=on,metacopy=on,verity=require" \
		"$OVL/merged"

	step "check the merged rootfs: expect file data served from the object store"
	local f
	f="$(find "$OVL/merged" -xdev -type f -size +0c -print -quit)"
	[ -n "$f" ] || fail "merged rootfs has no file content"
	cat "$f" >/dev/null || fail "reading $f through the verity overlay failed"
	touch "$OVL/merged/.e2e-rw-probe" || fail "merged overlay is not writable"

	"${KN[@]}" unmount "$OVL/merged"
	"${KN[@]}" unmount "$OVL/meta"
	[ -z "$(ls -A "$OVL/meta")" ] || fail "metadata mountpoint not empty after unmount"
	rm -rf "$OVL"
	OVL=""

	step "hold, then remove: expect the remove declined, naming the holder"
	"${KN[@]}" hold "$ref"
	local declined
	if declined="$("${KN[@]}" remove "$ref" 2>&1)"; then
		fail "remove succeeded while $ref was held"
	fi
	printf '%s\n' "$declined"
	printf '%s\n' "$declined" | grep -qF "held by cli" || fail "declined remove did not name the holder"
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed" | grep -qF "$ref" || fail "ls does not list $ref after the declined remove"

	step "release, then remove"
	"${KN[@]}" release "$ref"
	"${KN[@]}" remove "$ref"
	step "ls: expect the image gone"
	listed="$("${KN[@]}" ls)"
	printf '%s\n' "$listed"
	if printf '%s\n' "$listed" | grep -qF "$ref"; then fail "ls still lists $ref after remove"; fi
}

[ -x bin/ocicfs ] || make build

# Every phase talks to the run's own empty zot instance rather than the dev
# container's persistent one on 5000, so every assertion runs against
# hermetic per-run registry state untouched by manual testing or previous
# runs.
port="${E2E_REG_PORT:-5001}"
REGDIR="$(mktemp -d)"
cat >"$REGDIR/config.json" <<EOF
{
  "distSpecVersion": "1.1.1",
  "storage": { "rootDirectory": "$REGDIR/storage" },
  "http": { "address": "127.0.0.1", "port": "$port" }
}
EOF
zot serve "$REGDIR/config.json" >"$REGDIR/zot.log" 2>&1 &
REGPID=$!
for _ in $(seq 100); do
	curl -fsS "http://127.0.0.1:$port/v2/" >/dev/null 2>&1 && break
	sleep 0.1
done
curl -fsS "http://127.0.0.1:$port/v2/" >/dev/null 2>&1 \
	|| { cat "$REGDIR/zot.log" >&2; fail "zot did not become ready on port $port"; }

section "Build and seal a purpose-built image, cross-checked against composefs-rs and mkcomposefs"
# An offline, hardlink-free image covering the content classes and merge
# semantics the busybox legs cannot pin down: an object-sized and an
# inline-sized file, an empty file, a symlink, an empty directory, /run
# content the canonical transform drops, and a second layer carrying a
# whiteout and an override. Hardlink-free so the cross-check below must
# reach the exact four-way digest match, not its fallback.
step "build with umoci and push to the registry"
IMGDIR="$(mktemp -d /var/lib/ocicfs/e2e-img-XXXXXX)"
umoci init --layout "$IMGDIR/layout"
umoci new --image "$IMGDIR/layout:custom"
umoci unpack --image "$IMGDIR/layout:custom" "$IMGDIR/b1" >/dev/null 2>&1
r="$IMGDIR/b1/rootfs"
mkdir -p "$r/usr/bin" "$r/usr/share/empty" "$r/etc" "$r/run"
seq 1 40 >"$r/usr/bin/app" && chmod 0755 "$r/usr/bin/app"
printf 'inline config\n' >"$r/etc/config"
: >"$r/etc/empty"
ln -s ../../etc/config "$r/usr/share/link"
printf 'scratch\n' >"$r/run/junk"
printf 'doomed\n' >"$r/etc/doomed"
umoci repack --image "$IMGDIR/layout:custom" "$IMGDIR/b1" >/dev/null 2>&1
umoci unpack --image "$IMGDIR/layout:custom" "$IMGDIR/b2" >/dev/null 2>&1
rm "$IMGDIR/b2/rootfs/etc/doomed"
printf 'overridden\n' >"$IMGDIR/b2/rootfs/etc/config"
umoci repack --image "$IMGDIR/layout:custom" "$IMGDIR/b2" >/dev/null 2>&1
umoci config --image "$IMGDIR/layout:custom" --os linux --architecture "$(go env GOARCH)" >/dev/null 2>&1
umoci gc --layout "$IMGDIR/layout" >/dev/null 2>&1
cdigest="$(jq -r '.manifests[0].digest' "$IMGDIR/layout/index.json")"
crane push "$IMGDIR/layout" "127.0.0.1:$port/custom"
cref="127.0.0.1:$port/custom@$cdigest"

step "seal: expect one digest from the registry dry-run, the --from-dir dry-run and the --from-dir push"
cdry="$("${KN[@]}" seal --dry-run --format json "$cref" | jq -r .sealed)"
[[ "$cdry" == sha256:* ]] || fail "custom seal --dry-run reported no sealed digest"
cfromdir="$("${KN[@]}" seal --dry-run --from-dir "$IMGDIR/layout" --format json "$cref" | jq -r .sealed)"
[ "$cfromdir" = "$cdry" ] || fail "custom seal --from-dir dry-run digest differs from the registry dry-run"
csealed="$("${KN[@]}" seal --from-dir "$IMGDIR/layout" --format json "$cref" | jq -r .sealed)"
[ "$csealed" = "$cdry" ] || fail "custom sealed digest differs between dry-run and --from-dir push"
cman="$(crane manifest "127.0.0.1:$port/custom@$csealed")"
jq -e '.annotations["composefs.merged.erofs.v1.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$cman" >/dev/null || fail "custom sealed manifest lacks a well-formed merged fsverity annotation"
jq -e '.config.annotations["composefs.config.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$cman" >/dev/null || fail "custom sealed manifest lacks a well-formed config fsverity annotation"

step "seal-crosscheck: expect one merged fs-verity digest from ocicfs seal, composefs-rs (cfsctl oci compute-id), the Rust mkcomposefs (cfsctl mkcomposefs) and the C mkcomposefs over a umoci unpack"
ccheck="$(CROSSCHECK_REGISTRY="127.0.0.1:$port" scripts/seal-crosscheck.sh "$cref")"
printf '%s\n' "$ccheck" | grep -E "ocicfs:|cfsctl:|mkcomposefs:|RESULT"
printf '%s\n' "$ccheck" | grep -qF "RESULT: all four digests MATCH" \
	|| fail "seal-crosscheck did not reach the exact four-way match"

section "Lifecycle from registry: the sealed purpose-built image"
# The purpose-built image is a plain manifest, so it is its own identity.
run_lifecycle "127.0.0.1:$port/custom@$csealed" "$csealed" "$csealed" pull "127.0.0.1:$port/custom@$csealed"
rm -rf "$IMGDIR"
IMGDIR=""

section "Lifecycle from a local OCI directory (--from-dir)"
if [ ! -f "$LAYOUT/index.json" ]; then
	step "crane pull $IMAGE into $LAYOUT"
	crane pull --format=oci "$IMAGE" "$LAYOUT"
fi
digest="$(jq -r '.manifests[0].digest' "$LAYOUT/index.json")"
# The layout pins a multi-arch index, so the identity these legs must land on is
# the child manifest it offers for the host platform.
identity="$(host_child "$(cat "$LAYOUT/blobs/sha256/${digest#sha256:}")")"
[[ "$identity" == sha256:* ]] || fail "e2e layout index lists no linux/$(go env GOARCH) manifest"

run_lifecycle "e2e@$digest" "$identity" "$digest" pull --from-dir "$LAYOUT" "e2e@$digest"

section "Lifecycle from a multi-platform OCI tar (--from-tar)"
# The archive is laid out as docker save writes one: blobs before index.json,
# and the index entry carrying the reference docker save was given in its
# name annotation. No reference is passed: the pull takes the name from that
# annotation, drops its tag, and records it under the index digest, normalized
# as every reference is.
TARBALL="$(mktemp --suffix=.tar)"
TARINDEX="$(mktemp -d)"
jq '.manifests[0].annotations["io.containerd.image.name"] = "e2e:latest"' "$LAYOUT/index.json" >"$TARINDEX/index.json"
tar -C "$LAYOUT" -cf "$TARBALL" oci-layout blobs
tar -C "$TARINDEX" -rf "$TARBALL" index.json
run_lifecycle "index.docker.io/library/e2e@$digest" "$identity" "$digest" pull --from-tar "$TARBALL"

section "Lifecycle from a single-platform OCI tar (--from-tar)"
# The shape docker save --platform writes: index.json points straight at the
# platform manifest, so the recorded reference lands on that manifest and the
# identity is the manifest itself, with no index to descend.
if [ ! -f "$LAYOUT-single/index.json" ]; then
	step "crane pull --platform linux/$(go env GOARCH) $IMAGE into $LAYOUT-single"
	crane pull --format=oci --platform "linux/$(go env GOARCH)" "$IMAGE" "$LAYOUT-single"
fi
sdigest="$(jq -r '.manifests[0].digest' "$LAYOUT-single/index.json")"
[ "$(jq -r '.manifests[0].mediaType' "$LAYOUT-single/index.json")" = "application/vnd.oci.image.manifest.v1+json" ] \
	|| fail "single-platform layout's index.json does not point at a manifest"
jq '.manifests[0].annotations["io.containerd.image.name"] = "e2e:latest"' "$LAYOUT-single/index.json" >"$TARINDEX/index.json"
tar -C "$LAYOUT-single" -cf "$TARBALL" oci-layout blobs
tar -C "$TARINDEX" -rf "$TARBALL" index.json
run_lifecycle "index.docker.io/library/e2e@$sdigest" "$sdigest" "$sdigest" pull --from-tar "$TARBALL"

section "Lifecycle from a docker save or buildx archive (--from-tar)"
if [ -n "$ARCHIVE" ]; then
	step "archive $ARCHIVE"
	tar -tf "$ARCHIVE" 2>/dev/null | grep -qx 'index.json' \
		|| fail "$ARCHIVE is not an uncompressed OCI layout tar (no index.json listed; docker save and buildx write one, and ocicfs does not read a compressed archive)"
	aindex="$(tar -xOf "$ARCHIVE" index.json)"
	[ "$(jq -r '.manifests | length' <<<"$aindex")" = 1 ] \
		|| fail "$ARCHIVE holds $(jq -r '.manifests | length' <<<"$aindex") images; save exactly one image by name"
	aname="$(jq -r '.manifests[0].annotations["io.containerd.image.name"] // empty' <<<"$aindex")"
	[ -n "$aname" ] || fail "$ARCHIVE names no image (no io.containerd.image.name annotation); save the image by name, not by ID"
	adigest="$(jq -r '.manifests[0].digest' <<<"$aindex")"
	# The identity is the host child when the entry is an index, the entry
	# itself when it is a manifest.
	if [ "$(jq -r '.manifests[0].mediaType' <<<"$aindex")" = "application/vnd.oci.image.index.v1+json" ]; then
		aidentity="$(host_child "$(tar -xOf "$ARCHIVE" "blobs/sha256/${adigest#sha256:}")")"
		[[ "$aidentity" == sha256:* ]] || fail "$ARCHIVE's index lists no linux/$(go env GOARCH) manifest"
	else
		aidentity="$adigest"
	fi
	# The recorded reference is what the pull reports for the archive's name;
	# it must land on the entry digest, and the lifecycle checks ls, inspect
	# and the ref record agree with it.
	aref="$("${KN[@]}" pull --from-tar "$ARCHIVE" --format json | jq -r .ref)"
	echo "archive $ARCHIVE names $aname; recorded as $aref"
	[[ "$aref" == *"@$adigest" ]] || fail "recorded reference $aref does not pin the archive's entry $adigest"
	run_lifecycle "$aref" "$aidentity" "$adigest" pull --from-tar "$ARCHIVE"
else
	skip "E2E_ARCHIVE is unset"
fi

section "Lifecycle from registry by digest"
crane push "$LAYOUT" "127.0.0.1:$port/e2e"
run_lifecycle "127.0.0.1:$port/e2e@$digest" "$identity" "$digest" pull "127.0.0.1:$port/e2e@$digest"

section "Lifecycle from registry by tag"
run_lifecycle "127.0.0.1:$port/e2e:latest" "$identity" "$digest" pull "127.0.0.1:$port/e2e:latest"

section "Lifecycle from an ocicfs copy archive, unpinned and platform-pinned"
# copy keeps the blobs of the host platform only and lists the image twice in
# index.json, by the tag it was copied by and by the platform manifest, each
# name in both annotations. Pulled with no reference, the archive is the one
# image the index names; pinned to the platform manifest, it is that.
COPYTAR="$(mktemp --suffix=.tar)"
"${KN[@]}" copy -o "$COPYTAR" "127.0.0.1:$port/e2e:latest"
centries="$(tar -xOf "$COPYTAR" index.json | jq -c '[.manifests[] | [.digest, .annotations["io.containerd.image.name"], .annotations["org.opencontainers.image.ref.name"]]]')"
cwant="$(jq -nc --arg d "$digest" --arg i "$identity" --arg r "127.0.0.1:$port/e2e" \
	'[[$d, ($r + ":latest"), ($r + ":latest")], [$i, ($r + "@" + $i), ($r + "@" + $i)]]')"
[ "$centries" = "$cwant" ] || fail "ocicfs copy wrote the index.json entries $centries, want $cwant"
for child in $(jq -r --arg i "$identity" '.manifests[] | select(.digest != $i) | .digest' "$LAYOUT/blobs/sha256/${digest#sha256:}"); do
	! tar -tf "$COPYTAR" | grep -qx "blobs/sha256/${child#sha256:}" || fail "ocicfs copy wrote $child, the manifest of another platform"
done
run_lifecycle "127.0.0.1:$port/e2e@$digest" "$identity" "$digest" pull --from-tar "$COPYTAR"
run_lifecycle "127.0.0.1:$port/e2e@$identity" "$identity" "$identity" pull --from-tar "$COPYTAR" "127.0.0.1:$port/e2e@$identity"

section "Rootfs comparison: ocicfs mount against crane export of the same image"
# crane export flattens the image the same way ocicfs's merge must (docker
# export would not do: it dumps a *container*, whose runtime artifacts --
# .dockerenv, /etc/mtab, /dev stubs -- are not image content). The export is
# pinned to the host platform: ocicfs mounts the host-platform image, while
# crane resolves a multi-arch index to linux/amd64 by default on any host.
# Symlinks are compared as symlinks: real images carry links that only resolve
# in a running container (absolute targets, dpkg-trimmed doc trees), so
# dereferencing would produce false errors. diff checks names, content, and
# link targets; owners, modes, xattrs, and hardlink identity are covered by
# the Go e2e tests.
ref="127.0.0.1:$port/e2e@$digest"
"${KN[@]}" pull "$ref"
"${KN[@]}" mount "$ref" "$MNT"
EXPORTDIR="$(mktemp -d)"
crane --platform "linux/$(go env GOARCH)" export "$ref" - | tar -C "$EXPORTDIR" -x
diff -r -q --no-dereference "$EXPORTDIR" "$MNT" || fail "mounted rootfs differs from the flattened image"
"${KN[@]}" unmount "$MNT"
"${KN[@]}" remove "$ref"

section "Seal a registry image"
# Seal handles one platform per call; the e2e image is a multi-arch index,
# so select the host platform. --platform matches the index descriptors
# exactly, including the variant (busybox lists arm64 only as linux/arm64/v8),
# so read the full platform string from the index rather than assuming a bare
# os/arch pair. The printed sealed digest is the updated index's.
platform="$(crane manifest "$ref" | jq -r --arg arch "$(go env GOARCH)" \
	'first(.manifests[].platform | select(.os == "linux" and .architecture == $arch))
		| [.os, .architecture, .variant // empty] | join("/")')"
[[ "$platform" == linux/* ]] || fail "e2e index lists no linux/$(go env GOARCH) platform"
step "seal --dry-run: expect a sealed digest, nothing pushed"
dryrun="$("${KN[@]}" seal --dry-run --platform "$platform" --format json "$ref")"
sealed_dry="$(jq -r .sealed <<<"$dryrun")"
[[ "$sealed_dry" == sha256:* ]] || fail "seal --dry-run reported no sealed digest"
if crane manifest "127.0.0.1:$port/e2e@$sealed_dry" >/dev/null 2>&1; then
	fail "seal --dry-run pushed the sealed manifest"
fi

step "seal: expect the dry-run digest, pushed"
sealout="$("${KN[@]}" seal --platform "$platform" --format json "$ref")"
sealed="$(jq -r .sealed <<<"$sealout")"
[ "$sealed" = "$sealed_dry" ] || fail "sealed digest differs between dry-run and push"
[ "$sealed" != "$digest" ] || fail "sealing left the manifest digest unchanged"

step "check the sealed manifest: expect both fs-verity annotations as bare hex"
# The annotations live on each platform image manifest. A multi-arch image
# seals as an index, so descend to the host platform's child first.
manifest="$(crane manifest "127.0.0.1:$port/e2e@$sealed")"
child="$sealed"
if jq -e '.manifests' <<<"$manifest" >/dev/null; then
	child="$(host_child "$manifest")"
	[[ "$child" == sha256:* ]] || fail "sealed index has no host-platform manifest"
	manifest="$(crane manifest "127.0.0.1:$port/e2e@$child")"
fi
jq -e '.annotations["composefs.merged.erofs.v1.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$manifest" >/dev/null || fail "sealed manifest lacks a well-formed merged fsverity annotation"
jq -e '.config.annotations["composefs.config.fsverity-sha256-12"] | test("^[0-9a-f]{64}$")' \
	<<<"$manifest" >/dev/null || fail "sealed manifest lacks a well-formed config fsverity annotation"

section "Lifecycle from registry: the sealed image, pull re-verifying the seal"
# $child is the sealed image's identity: the host platform's manifest when the
# seal produced an index, and the sealed manifest itself otherwise.
run_lifecycle "127.0.0.1:$port/e2e@$sealed" "$child" "$sealed" pull "127.0.0.1:$port/e2e@$sealed"

section "Seal enforcement: mount trusts the annotation over a tampered binding"
# The lifecycle legs removed their images, so re-pull the two sealed ones:
# the multi-arch e2e image is the mount under test, and the purpose-built
# image supplies a different, internally consistent object for the
# repointing check. Last leg by design -- it leaves knowingly wrong bindings
# behind between assertions.
sref="127.0.0.1:$port/e2e@$sealed"
"${KN[@]}" pull "$sref"
"${KN[@]}" pull "127.0.0.1:$port/custom@$csealed"
# Each identity was pulled for exactly one platform, so its binding is the
# single JSON file in the identity's directory. Globbing sidesteps the
# binding filename's platform normalization (a default variant like
# arm64/v8 is canonicalized away).
shex="${child#sha256:}"
sbind=$(echo "$STORE/images/sha256/${shex:0:2}/${shex:2}/"*.json)
chex="${csealed#sha256:}"
cbind=$(echo "$STORE/images/sha256/${chex:0:2}/${chex:2}/"*.json)
[ -f "$sbind" ] || fail "no sealed-image binding at $sbind"
[ -f "$cbind" ] || fail "no purpose-built binding at $cbind"
recorded="$(jq -r .rootfs.fsverity.digest "$sbind")"

step "tamper with the binding digest and mount: expect the mount to trust the seal"
tmp="$(mktemp)"
jq '.rootfs.fsverity.digest = "sha256:'"$(printf '0%.0s' {1..64})"'"' "$sbind" >"$tmp" && mv "$tmp" "$sbind"
"${KN[@]}" mount "$sref" "$MNT" || fail "mount did not trust the sealing annotation over the tampered binding"
[ -n "$(ls -A "$MNT")" ] || fail "sealed mount is empty"
"${KN[@]}" unmount "$MNT"

step "pull again: expect the tampered binding healed"
"${KN[@]}" pull "$sref"
[ "$(jq -r .rootfs.fsverity.digest "$sbind")" = "$recorded" ] || fail "re-pull did not restore the binding digest"

step "repoint the binding and mount: expect the mount to fail on the seal"
tmp="$(mktemp)"
jq --slurpfile o "$cbind" \
	'.rootfs.descriptor = $o[0].rootfs.descriptor | .rootfs.fsverity.digest = $o[0].rootfs.fsverity.digest' \
	"$sbind" >"$tmp" && mv "$tmp" "$sbind"
if out="$("${KN[@]}" mount "$sref" "$MNT" 2>&1)"; then
	fail "mount succeeded on a binding repointed at another object"
fi
printf '%s\n' "$out"
printf '%s\n' "$out" | grep -qF "sealing annotation claims" || fail "repointed-binding mount did not name the sealing annotation"

step "pull again: expect it to fail closed on the disagreement"
# A repointed binding is an integrity disagreement, not staleness, so a plain
# re-pull must refuse rather than paper over it; remove-then-pull is the
# remedy.
if out="$("${KN[@]}" pull "$sref" 2>&1)"; then
	fail "plain pull succeeded over a repointed binding"
fi
printf '%s\n' "$out"
printf '%s\n' "$out" | grep -qF "annotation" || fail "repointed-binding pull did not name the sealing annotation"

step "remove, pull and mount: expect a clean mount again"
"${KN[@]}" remove "$sref"
"${KN[@]}" pull "$sref"
"${KN[@]}" mount "$sref" "$MNT"
[ -n "$(ls -A "$MNT")" ] || fail "re-pulled sealed mount is empty"
"${KN[@]}" unmount "$MNT"
"${KN[@]}" remove "$sref"
"${KN[@]}" remove "127.0.0.1:$port/custom@$csealed"

section "Garbage collection: reclamation and mount protection"
step "gc: expect the unwanted image reclaimed, the survivor kept"
"${KN[@]}" pull "$sref"
"${KN[@]}" pull "127.0.0.1:$port/custom@$csealed"
"${KN[@]}" remove "127.0.0.1:$port/custom@$csealed"
"${KN[@]}" gc --dry-run
out="$("${KN[@]}" gc --format json)"
printf '%s\n' "$out"
[ "$(printf '%s' "$out" | jq -r .objectsRemoved)" -gt 0 ] || fail "gc reclaimed nothing after remove"
"${KN[@]}" mount "$sref" "$MNT"
f="$(find "$MNT" -type f -size +64c -print -quit)"
[ -n "$f" ] || fail "survivor mount has no by-digest file after gc"
cat "$f" >/dev/null || fail "reading survivor content failed after gc"
"${KN[@]}" unmount "$MNT"

step "gc with a metadata-only mount: expect the image kept until unmount, then reclaimed"
"${KN[@]}" mount --metadata-only "$sref" "$MNT"
"${KN[@]}" remove "$sref"
out="$("${KN[@]}" gc --format json)"
printf '%s\n' "$out"
[ "$(printf '%s' "$out" | jq -r .inUseSkipped)" -gt 0 ] || fail "gc did not see the mounted image as in use"
[ -n "$(find "$STORE/objects/sha256" -type f -print -quit)" ] || fail "in-use image's objects were swept"
[ -n "$(ls -A "$MNT")" ] || fail "metadata mount broke during gc"
# Data objects open lazily: prove a by-digest file still opens through the
# live data plane after the collection, not merely that files remain on disk.
GCOVL="$(mktemp -d /var/lib/ocicfs/e2e-gc-ovl-XXXXXX)"
mkdir "$GCOVL/merged" "$GCOVL/upper" "$GCOVL/work"
mount -t overlay overlay -o \
	"lowerdir=$MNT::$STORE/objects/fsverity-sha256-12,upperdir=$GCOVL/upper,workdir=$GCOVL/work,redirect_dir=on,metacopy=on,verity=require" \
	"$GCOVL/merged"
f="$(find "$GCOVL/merged" -xdev -type f -size +64c -print -quit)"
[ -n "$f" ] || fail "post-gc merged rootfs has no by-digest file"
cat "$f" >/dev/null || fail "reading $f through the post-gc overlay failed"
"${KN[@]}" unmount "$GCOVL/merged"
rm -rf "$GCOVL"
GCOVL=""
"${KN[@]}" unmount "$MNT"
out="$("${KN[@]}" gc --format json)"
printf '%s\n' "$out"
[ "$(printf '%s' "$out" | jq -r .objectsRemoved)" -gt 0 ] || fail "gc after unmount reclaimed nothing"
[ -z "$(find "$STORE/objects/sha256" -type f)" ] || fail "objects remain in an empty store"
[ -z "$(find "$STORE/objects/fsverity-sha256-12" -type f)" ] || fail "view entries remain in an empty store"
for tree in "$STORE/objects/sha256" "$STORE/objects/fsverity-sha256-12" \
	"$STORE/images/sha256" "$STORE/refs/sha256" "$STORE/holds/sha256"; do
	[ -z "$(find "$tree" -mindepth 1 -type d 2>/dev/null)" ] || fail "empty shard directories remain under $tree"
done

step "gc with a plain mount: expect the image kept until unmount, then reclaimed"
"${KN[@]}" pull "$sref"
"${KN[@]}" mount "$sref" "$MNT"
"${KN[@]}" remove "$sref"
out="$("${KN[@]}" gc --format json)"
printf '%s\n' "$out"
[ "$(printf '%s' "$out" | jq -r .inUseSkipped)" -gt 0 ] || fail "gc did not see the plain-mounted image as in use"
# Data objects open lazily through the mount's data plane: prove a by-digest
# file still opens through the live mount after the collection.
f="$(find "$MNT" -xdev -type f -size +64c -print -quit)"
[ -n "$f" ] || fail "post-gc plain mount has no by-digest file"
cat "$f" >/dev/null || fail "reading $f through the plain mount after gc failed"
"${KN[@]}" unmount "$MNT"
# The mount's loop device detaches via autoclear, which is deferred kernel
# work after the unmount, so poll briefly before asserting cleanliness.
for _ in $(seq 100); do
	losetup -ln -O BACK-FILE 2>/dev/null | grep -qF "$STORE" || break
	sleep 0.1
done
if losetup -ln -O BACK-FILE 2>/dev/null | grep -qF "$STORE"; then
	fail "a loop device still backs the store after unmount"
fi
out="$("${KN[@]}" gc --format json)"
printf '%s\n' "$out"
[ "$(printf '%s' "$out" | jq -r .objectsRemoved)" -gt 0 ] || fail "gc after plain unmount reclaimed nothing"
[ -z "$(find "$STORE/objects/sha256" -type f)" ] || fail "objects remain in an empty store"

section "Delta pull from registry: ranges over a held N, same image as a plain pull"
# Two releases sharing their base layer, published zstd:chunked: N+1 changes
# one inline file, removes one file and adds one, so its delta is a couple of
# ranges. The fixtures are linux/amd64 images; a host of another
# architecture cannot pull them.
if [ "$(go env GOARCH)" = amd64 ]; then
	fixtures=internal/zstdchunked/testdata
	crane push "$fixtures/release-n" "127.0.0.1:$port/release:n"
	crane push "$fixtures/release-n1" "127.0.0.1:$port/release:n1"
	ndigest="$(jq -r '.manifests[0].digest' "$fixtures/release-n/index.json")"
	n1digest="$(jq -r '.manifests[0].digest' "$fixtures/release-n1/index.json")"
	nref="127.0.0.1:$port/release@$ndigest"
	n1ref="127.0.0.1:$port/release@$n1digest"
	# app/old is the one file only N carries; its object must go with N.
	oldhex="$(printf 'this file is bigger than sixty-four bytes so it is stored as an object.\nold\n' | sha256sum | cut -d' ' -f1)"
	oldobj="$STORE/objects/sha256/${oldhex:0:2}/${oldhex:2}"

	step "pull --delta N into the empty store and hold it as delta-base"
	"${KN[@]}" pull --delta "$nref"
	[ -f "$oldobj" ] || fail "N's app/old object is not in the store after the delta pull"
	"${KN[@]}" hold --holder delta-base "$nref"

	step "pull --delta N+1: expect every layer by ranges, fewer bytes than the layers"
	dlog="$(mktemp)"
	"${KN[@]}" pull --delta "$n1ref" 2>"$dlog"
	cat "$dlog"
	applayer="$(jq -r '.layers[1].digest' "$fixtures/release-n1/blobs/sha256/${n1digest#sha256:}")"
	grep -E "msg=\"delta: layer\" layer=$applayer .* requests=[1-9][0-9]* .* fallback=none" "$dlog" >/dev/null \
		|| fail "the app layer was not fetched by ranges"
	[ "$(grep -c 'msg="delta: layer"' "$dlog")" -eq 2 ] || fail "expected one delta log line per layer"
	if grep 'msg="delta: layer"' "$dlog" | grep -qv 'fallback=none'; then fail "a layer fell back to a whole fetch"; fi
	summary="$(grep 'msg="delta: pull"' "$dlog")"
	[ -n "$summary" ] || fail "no delta summary line logged"
	fetched="$(sed -n 's/.* fetched=\([0-9]*\).*/\1/p' <<<"$summary")"
	size="$(sed -n 's/.* size=\([0-9]*\).*/\1/p' <<<"$summary")"
	[ "$fetched" -gt 0 ] && [ "$fetched" -lt "$size" ] || fail "delta pull fetched $fetched of $size layer bytes"
	rm -f "$dlog"

	step "pull N+1 plainly into a second store: expect the same composefs image"
	DSTORE="$(mktemp -d /var/lib/ocicfs/e2e-delta-XXXXXX)"
	bin/ocicfs --root "$DSTORE" pull "$n1ref"
	dinspect="$("${KN[@]}" inspect --format json "$n1ref")"
	pinspect="$(bin/ocicfs --root "$DSTORE" inspect --format json "$n1ref")"
	[ "$(jq -r .composefs.digest <<<"$dinspect")" = "$(jq -r .composefs.digest <<<"$pinspect")" ] \
		|| fail "the delta pull built a different composefs image than the plain pull"
	[ "$(jq -r .fsverity <<<"$dinspect")" = "$(jq -r .fsverity <<<"$pinspect")" ] \
		|| fail "the delta pull's fsverity digest differs from the plain pull's"
	rm -rf "$DSTORE"
	DSTORE=""

	step "release delta-base, remove N, gc: expect N's own object gone, N+1 intact"
	"${KN[@]}" release --holder delta-base "$nref"
	"${KN[@]}" remove "$nref"
	out="$("${KN[@]}" gc --format json)"
	printf '%s\n' "$out"
	[ "$(printf '%s' "$out" | jq -r .objectsRemoved)" -gt 0 ] || fail "gc reclaimed nothing of N"
	[ ! -e "$oldobj" ] || fail "N's app/old object survived gc"
	"${KN[@]}" mount "$n1ref" "$MNT"
	[ "$(cat "$MNT/app/config")" = "v=2" ] || fail "N+1's app/config is not the delta-fetched content"
	cat "$MNT/app/new" >/dev/null || fail "reading N+1's app/new after gc failed"
	[ ! -e "$MNT/app/old" ] || fail "N+1 still shows app/old"
	"${KN[@]}" unmount "$MNT"
	"${KN[@]}" remove "$n1ref"
else
	skip "the delta fixtures are linux/amd64 images"
fi

section "fs-verity build modes: the state on tmpfs and on the verity loopback"
# The build mode alone decides whether objects get fs-verity and mounts
# require it; an auto build probes the store's filesystem. Nothing reads a
# mode from the objects: a required build meeting an object written without
# fs-verity fails where it measures it, naming the remedy. The required and
# off binaries are built here from the same tree.
VDIR="$(mktemp -d /var/lib/ocicfs/e2e-modes-XXXXXX)"
mkdir "$VDIR/tmpfs"
mount -t tmpfs tmpfs "$VDIR/tmpfs"
for mode in required off; do
	CGO_ENABLED=0 go build -tags "ocicfs_verity_$mode" -o "$VDIR/ocicfs-$mode" ./cmd/ocicfs
done
VAUTO=bin/ocicfs
VREQ="$VDIR/ocicfs-required"
VOFF="$VDIR/ocicfs-off"
vref="e2e@$digest"
MISMATCH="carries no fs-verity, which this build requires"

# expect_state checks the verity line binary $1, built in mode $2, prints
# for store $3 against the state $4, and that every object in the store
# carries fs-verity exactly when $4 is enabled.
expect_state() {
	local bin="$1" build="$2" store="$3" want="$4" out obj
	out="$("$bin" --root "$store" version)"
	printf '%s\n' "$out"
	printf '%s\n' "$out" | grep -qx "verity: $want (build: $build)" || fail "version does not report verity: $want (build: $build)"
	while read -r obj; do
		if fsverity measure "$obj" >/dev/null 2>&1; then
			[ "$want" = enabled ] || fail "object $obj in the $want store has fs-verity"
		else
			[ "$want" = disabled ] || fail "object $obj in the $want store has no fs-verity"
		fi
	done < <(find "$store/objects/sha256" -type f)
}

# mount_and_read mounts the image with binary $1 from store $2 and reads an
# object-backed file through it.
mount_and_read() {
	local bin="$1" store="$2" f
	"$bin" --root "$store" mount "$vref" "$MNT"
	f="$(find "$MNT" -xdev -type f -size +64c -print -quit)"
	[ -n "$f" ] || fail "the mount from $store has no object-backed file"
	cat "$f" >/dev/null || fail "reading $f through the mount from $store failed"
	"$bin" --root "$store" unmount "$MNT"
}

# expect_mount_mismatch asserts binary $1 fails to mount from store $2 naming
# the fs-verity mismatch.
expect_mount_mismatch() {
	local bin="$1" store="$2" out
	if out="$("$bin" --root "$store" mount "$vref" "$MNT" 2>&1)"; then
		"$bin" --root "$store" unmount "$MNT"
		fail "$bin mounted from $store, whose objects carry no fs-verity"
	fi
	printf '%s\n' "$out"
	printf '%s\n' "$out" | grep -qF "$MISMATCH" || fail "the refused mount did not name the fs-verity mismatch"
	! mountpoint -q "$MNT" || fail "the refused mount left $MNT mounted"
}

step "auto on tmpfs: expect verity disabled, then pull, mount and read"
expect_state "$VAUTO" auto "$VDIR/tmpfs/auto" disabled
"$VAUTO" --root "$VDIR/tmpfs/auto" pull --from-dir "$LAYOUT" "$vref"
expect_state "$VAUTO" auto "$VDIR/tmpfs/auto" disabled
mount_and_read "$VAUTO" "$VDIR/tmpfs/auto"
out="$("$VAUTO" --root "$VDIR/tmpfs/auto" inspect "$vref")"
printf '%s\n' "$out" | grep -E '^verity '
printf '%s\n' "$out" | grep -qE '^verity +disabled \(build: auto\)$' || fail "inspect does not report verity disabled (build: auto)"

step "auto on the verity loopback: expect verity enabled"
"$VAUTO" --root "$VDIR/auto" pull --from-dir "$LAYOUT" "$vref"
expect_state "$VAUTO" auto "$VDIR/auto" enabled

step "required on tmpfs: expect verity unsupported, the pull to fail and nothing written"
expect_state "$VREQ" required "$VDIR/tmpfs/required" unsupported
[ -z "$(find "$VDIR/tmpfs/required/objects" -type f -print -quit)" ] || fail "the version report wrote objects"
if out="$("$VREQ" --root "$VDIR/tmpfs/required" pull --from-dir "$LAYOUT" "$vref" 2>&1)"; then
	fail "a required build pulled into a store without fs-verity"
fi
printf '%s\n' "$out"
printf '%s\n' "$out" | grep -qF "fs-verity-capable filesystem" || fail "the refused pull did not name the missing fs-verity"
[ -z "$(find "$VDIR/tmpfs/required/objects" -type f -print -quit)" ] || fail "the refused pull wrote objects"

step "off on the verity loopback: expect verity disabled, then mount and read"
"$VOFF" --root "$VDIR/off" pull --from-dir "$LAYOUT" "$vref"
expect_state "$VOFF" off "$VDIR/off" disabled
mount_and_read "$VOFF" "$VDIR/off"

step "required and auto on the store the off build filled: expect the mount refused, naming the mismatch"
"$VREQ" --root "$VDIR/off" ls >/dev/null || fail "a required build cannot list a store written without fs-verity"
expect_mount_mismatch "$VREQ" "$VDIR/off"
expect_mount_mismatch "$VAUTO" "$VDIR/off"

# enable_in_place enables fs-verity in place on the objects of store $1: it
# checks every object's content digest against its name, then enables
# fs-verity on each with the parameters ocicfs digests use.
enable_in_place() {
	(
		cd "$1/objects/sha256" &&
			for f in */*; do
				[ "$(sha256sum <"$f" | cut -d' ' -f1)" = "${f%%/*}${f#*/}" ] ||
					{ echo "$f does not match its name" >&2; exit 1; }
			done &&
			for f in */*; do
				fsverity measure "$f" >/dev/null 2>&1 ||
					fsverity enable --hash-alg=sha256 --block-size=4096 "$f" ||
					exit 1
			done
	)
}

step "fs-verity enabled in place on a corrupted object: expect the conversion refused, nothing enabled"
vobj="$(find "$VDIR/off/objects/sha256" -type f -size +64c -print -quit)"
cp -p "$vobj" "$VDIR/saved-object"
head -c "$(stat -c %s "$vobj")" /dev/zero >"$vobj"
if enable_in_place "$VDIR/off"; then
	fail "the conversion accepted an object that does not match its name"
fi
expect_state "$VOFF" off "$VDIR/off" disabled
cp -p "$VDIR/saved-object" "$vobj"

step "fs-verity enable failing on the first object: expect the conversion to fail"
vfirst="$(cd "$VDIR/off/objects/sha256" && printf '%s\n' */* | head -n1)"
# The kernel refuses to enable fs-verity on a file open for writing.
exec 9>>"$VDIR/off/objects/sha256/$vfirst"
if enable_in_place "$VDIR/off"; then
	fail "the conversion succeeded although enabling fs-verity on $vfirst failed"
fi
exec 9>&-

step "fs-verity enabled in place on the off build's objects: expect required and auto to mount and read"
enable_in_place "$VDIR/off" || fail "the in-place conversion failed"
expect_state "$VREQ" required "$VDIR/off" enabled
mount_and_read "$VREQ" "$VDIR/off"
mount_and_read "$VAUTO" "$VDIR/off"

step "off on the store the auto build filled: expect the objects with fs-verity read"
mount_and_read "$VOFF" "$VDIR/auto"
