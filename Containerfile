# syntax=docker/dockerfile:1.7
#
# knarr dev / CI container image.
#
# One image, built by .devcontainer/devcontainer.json for local development
# and by the CI workflow (via the devcontainers/ci action) for build/test/lint,
# so the Go toolchain and lint tooling are identical for developers and CI.
#
# Stage 1 (composefs-builder) compiles composefs tools from a pinned composefs
# release. Stage 2 (the dev image) inherits the Go toolchain base, copies the
# composefs artifacts across, and adds the loop-device / ext4 tooling the
# privileged fs-verity end-to-end tests need. The entrypoint mounts a
# loop-backed ext4 filesystem with the verity feature so those tests get real
# fs-verity; run the container privileged for that to succeed.

ARG GO_VERSION=1.26.1
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
# Stage 2 — dev image.
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
 && rm -rf /var/lib/apt/lists/*

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
    | sh -s -- -b "$(go env GOPATH)/bin" "${GOLANGCI_LINT_VERSION}" \
 && golangci-lint --version

# Pull mkcomposefs + mount.composefs + composefs-info + libcomposefs.so from the builder stage.
# ldconfig rebuilds the linker cache so the shared library is discoverable; the
# smoke tests confirm the binaries are on PATH and link against it. composefs-info
# inspects and measures a built image; the EROFS round-trip test uses its dump and
# measure-file modes to assert WriteImage reproduces the upstream tool's output.
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
