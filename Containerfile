# syntax=docker/dockerfile:1.7
#
# knarr dev / CI container image.
#
# One image, built by .devcontainer/devcontainer.json for local development
# and by the CI workflow (via the devcontainers/ci action) for build/test/lint,
# so the Go toolchain and lint tooling are identical for developers and CI.
#
# Stage 1 (composefs-builder) compiles composefs tools from a pinned composefs
# release. Stage 2 (cfsctl-builder) compiles cfsctl from a pinned composefs-rs
# commit — the cross-implementation oracle the canonical-EROFS conformance test
# measures knarr against. Stage 3 (the dev image) inherits the Go toolchain base,
# copies both sets of artifacts across, and adds the loop-device / ext4 tooling
# the privileged fs-verity end-to-end tests need. The entrypoint mounts a
# loop-backed ext4 filesystem with the verity feature so those tests get real
# fs-verity; run the container privileged for that to succeed.

ARG GO_VERSION=1.26.1
ARG RUST_VERSION=1.88.0
ARG DEBIAN_RELEASE=bookworm

# ------------------------------------------------------------------
# Stage 1 — composefs-builder: build composefs tools from the pinned release.
# ------------------------------------------------------------------
FROM debian:${DEBIAN_RELEASE}-slim AS composefs-builder

# The tag is convenient to read, but tags are mutable; COMPOSEFS_COMMIT is the
# immutable commit v1.0.8 resolves to and the build asserts the clone landed on
# it, so a moved tag fails the build rather than silently changing the tool that
# defines the EROFS format.
ARG COMPOSEFS_VERSION=v1.0.8
ARG COMPOSEFS_COMMIT=858ce1b38e1534c2602eb431124b5dca706bc746

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        meson \
        ninja-build \
        pkg-config \
        build-essential \
        libssl-dev \
 && rm -rf /var/lib/apt/lists/*

RUN MULTIARCH="$(dpkg-architecture -qDEB_HOST_MULTIARCH)" \
 && git clone --depth 1 --branch "${COMPOSEFS_VERSION}" \
        https://github.com/containers/composefs.git /tmp/composefs \
 && test "$(git -C /tmp/composefs rev-parse HEAD)" = "${COMPOSEFS_COMMIT}" \
 && meson setup /tmp/composefs/build /tmp/composefs \
        --prefix=/usr \
        --libdir="/usr/lib/${MULTIARCH}" \
        -Dfuse=disabled \
        -Dman=disabled \
 && meson compile -C /tmp/composefs/build \
 && DESTDIR=/out meson install -C /tmp/composefs/build

# ------------------------------------------------------------------
# Stage 2 — cfsctl-builder: build cfsctl from a pinned composefs-rs commit.
# ------------------------------------------------------------------
# cfsctl is the cross-implementation oracle for the canonical merged-EROFS
# construction: the conformance test compares its `oci compute-id` against the
# merged image knarr builds for the same layers. The commit is pinned (an
# arbitrary SHA, fetched directly) so the oracle cannot drift, and the build
# asserts the checkout landed on it. Only the `oci` feature is enabled — no
# containers-storage / ostree / fuse — to keep the stage light; the `oci:`
# transport reads local OCI layouts without skopeo. Rust exists only in this
# build stage; the dev image copies just the binary, so the device stays
# Rust-free.
FROM rust:${RUST_VERSION}-${DEBIAN_RELEASE} AS cfsctl-builder

ARG COMPOSEFS_RS_COMMIT=e836ce4f0f944663e6afdad1817c6061bd548751

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        pkg-config \
        libssl-dev \
        zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*

RUN git init /tmp/composefs-rs \
 && git -C /tmp/composefs-rs remote add origin https://github.com/composefs/composefs-rs.git \
 && git -C /tmp/composefs-rs fetch --depth 1 origin "${COMPOSEFS_RS_COMMIT}" \
 && git -C /tmp/composefs-rs checkout --quiet FETCH_HEAD \
 && test "$(git -C /tmp/composefs-rs rev-parse HEAD)" = "${COMPOSEFS_RS_COMMIT}"

RUN cargo build --release \
        --manifest-path /tmp/composefs-rs/Cargo.toml \
        -p composefs-ctl --no-default-features --features oci \
 && cp /tmp/composefs-rs/target/release/cfsctl /usr/bin/cfsctl

# ------------------------------------------------------------------
# Stage 3 — dev image.
# ------------------------------------------------------------------
FROM golang:${GO_VERSION}-${DEBIAN_RELEASE}

# Pinned golangci-lint (bundles the gofumpt formatter). Keep in sync with
# .golangci.yml and the make lint target.
ARG GOLANGCI_LINT_VERSION=v2.11.4

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        make \
        libssl3 \
        # tree + jq: inspect the store / OCI-layout structure and read manifest
        # and binding JSON while developing.
        tree \
        jq \
        # fs-verity e2e runtime: losetup (util-linux) for the loop-backed test
        # image and mkfs.ext4 (e2fsprogs) to format it with the verity feature.
        util-linux \
        e2fsprogs \
        # fsverity: the reference fs-verity-utils CLI, a userspace oracle the
        # verity digest test measures against alongside the kernel measure.
        fsverity \
 && rm -rf /var/lib/apt/lists/* \
 && command -v fsverity >/dev/null

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
    | sh -s -- -b "$(go env GOPATH)/bin" "${GOLANGCI_LINT_VERSION}" \
 && golangci-lint --version

# Pull mkcomposefs + mount.composefs + composefs-info + libcomposefs.so from the builder stage.
# ldconfig rebuilds the linker cache so the shared library is discoverable; the
# smoke tests confirm the binaries are on PATH and link against it. composefs-info
# inspects and measures a built image; the EROFS round-trip test uses its dump and
# measure-file modes to assert WriteImage reproduces the upstream tool's output.
# mount.composefs ships with the tools but is no longer used: knarr assembles
# its mounts natively.
COPY --from=composefs-builder /out/usr/bin/mkcomposefs /usr/bin/mkcomposefs
COPY --from=composefs-builder /out/usr/sbin/mount.composefs /usr/bin/mount.composefs
COPY --from=composefs-builder /out/usr/bin/composefs-info /usr/bin/composefs-info
COPY --from=composefs-builder /out/usr/lib/ /usr/lib/
RUN ldconfig \
 && command -v mkcomposefs >/dev/null \
 && command -v mount.composefs >/dev/null \
 && command -v composefs-info >/dev/null \
 && ldd /usr/bin/mkcomposefs | grep -q libcomposefs

# crane: a lightweight, single-binary tool to pull a registry image into a local
# OCI image layout for `knarr pull --from-dir`. Copied from its official static
# image rather than building skopeo, which is far heavier (C deps, ~2x the size).
COPY --from=gcr.io/go-containerregistry/crane:v0.21.7 /ko-app/crane /usr/bin/crane
RUN command -v crane >/dev/null

# cfsctl: the pinned composefs-rs oracle the canonical-EROFS conformance test
# compares knarr's merged image against. Built with only the `oci` feature; every
# shared library it links (zlib, pcre2, libgcc, libm, libc) is already present in
# the base image, so no extra runtime packages are needed.
COPY --from=cfsctl-builder /usr/bin/cfsctl /usr/bin/cfsctl
RUN command -v cfsctl >/dev/null

# zot: a real OCI registry, statically linked, for the smoke test's registry
# legs and for manual push/seal/pull testing. Unlike an in-process test
# registry it enforces the distribution spec on manifest PUTs (Content-Type
# handling, referenced-blob existence), so the `knarr seal` push path is
# exercised against real registry behavior. The entrypoint serves a persistent
# instance at 127.0.0.1:5000 from the baked config; the smoke test runs its
# own ephemeral instance on another port.
ARG ZOT_VERSION=2.1.17
RUN arch="$(dpkg --print-architecture)" \
 && case "$arch" in amd64|arm64) ;; *) echo "unsupported architecture for zot: $arch" >&2; exit 1 ;; esac \
 && curl -sSfL -o /usr/bin/zot \
        "https://github.com/project-zot/zot/releases/download/v${ZOT_VERSION}/zot-linux-${arch}" \
 && chmod 0755 /usr/bin/zot \
 && zot --version
COPY .devcontainer/zot-config.json /etc/zot/config.json

# umoci: a pinned OCI image unpacker, the independent flattener the
# seal-crosscheck script uses for its third comparison leg. Built from the
# pinned tag's vendored sources (release binaries are amd64-only and
# `go install` rejects its replace directives); the build asserts the
# checkout landed on the pinned commit.
ARG UMOCI_VERSION=v0.4.7
ARG UMOCI_COMMIT=17f38511d61846e2fb8ec01a1532f3ef5525e71d
RUN git clone --depth 1 --branch "${UMOCI_VERSION}" https://github.com/opencontainers/umoci /tmp/umoci \
 && test "$(git -C /tmp/umoci rev-parse HEAD)" = "${UMOCI_COMMIT}" \
 && (cd /tmp/umoci && go build -mod=vendor -o /usr/bin/umoci ./cmd/umoci) \
 && rm -rf /tmp/umoci \
 && umoci --version

# Share the toolchain caches with bind-mounted workspaces.
ENV GOMODCACHE=/go/pkg/mod
ENV GOCACHE=/root/.cache/go-build

# The mounted workspace is owned by the host user, not container root; mark
# any workspace path safe so `go build` can read VCS info for the version
# stamp (the devcontainer mounts it under /workspaces, a direct run under /work).
RUN git config --global --add safe.directory '*'

COPY .devcontainer/dev-entrypoint.sh /usr/local/bin/dev-entrypoint
RUN chmod +x /usr/local/bin/dev-entrypoint

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/dev-entrypoint"]
CMD ["/bin/bash"]
