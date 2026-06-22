# syntax=docker/dockerfile:1.7
#
# knarr dev / CI container image.
#
# One image, built by .devcontainer/devcontainer.json for local development
# and by the CI workflow (via the devcontainers/ci action) for build/test/lint,
# so the Go toolchain and lint tooling are identical for developers and CI.
#
# The current build/test/lint/vet targets need only the Go toolchain plus
# golangci-lint. Milestones that add mount / fs-verity integration tests will
# extend this image with mkcomposefs and grant the devcontainer a privileged
# path; until then it stays lean and unprivileged.

ARG GO_VERSION=1.26.1
ARG DEBIAN_RELEASE=bookworm

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
 && rm -rf /var/lib/apt/lists/*

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
    | sh -s -- -b "$(go env GOPATH)/bin" "${GOLANGCI_LINT_VERSION}" \
 && golangci-lint --version

# Share the toolchain caches with bind-mounted workspaces.
ENV GOMODCACHE=/go/pkg/mod
ENV GOCACHE=/root/.cache/go-build

# The mounted workspace is owned by the host user, not container root; mark
# any workspace path safe so `go build` can read VCS info for the version
# stamp (the devcontainer mounts it under /workspaces, a direct run under /work).
RUN git config --global --add safe.directory '*'

WORKDIR /work
CMD ["/bin/bash"]
