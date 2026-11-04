# SPDX-License-Identifier: Apache-2.0

GO ?= go
GOFLAGS ?=
GOLANGCI_LINT ?= golangci-lint

# Build artifacts live under bin/.
BIN := bin
CMD := ocicfs
PKG := github.com/foundriesio/ocicfs

# Install location (overridable, e.g. DESTDIR for packaging).
PREFIX ?= /usr/local
DESTDIR ?=

# Device-footprint budget for the ocicfs binary, in bytes. The total device
# budget is 25 MB including mkcomposefs; the size target checks the binary
# against this slice of it.
SIZE_BUDGET ?= 20971520

# Version stamping: derive from git, fall back to "dev"/"none".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)

# -s -w strip the symbol table and DWARF for a smaller device binary; the -X
# flags stamp the version. Builds use -trimpath for reproducibility.
LDFLAGS := -s -w -X $(PKG)/pkg/version.Version=$(VERSION) -X $(PKG)/pkg/version.Revision=$(REVISION)

.PHONY: all
all: build

# check runs the formatting check, the linters, go vet, and the unit tests.
.PHONY: check
check: lint vet test

# build compiles the ocicfs CLI into bin/ as a stripped, trimmed binary. Cgo is
# disabled so the binary is fully static: nothing in ocicfs needs it, and the
# release artifacts must not pick up a libc dependency from the build host.
.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$(CMD) ./cmd/$(CMD)

# size builds the stripped binary and checks it against the device budget so the
# footprint is tracked with data rather than estimates.
.PHONY: size
size: build
	@bytes=$$(wc -c < $(BIN)/$(CMD)); \
	printf 'ocicfs binary: %s bytes (%s MiB); budget %s bytes\n' \
		"$$bytes" "$$((bytes / 1024 / 1024))" "$(SIZE_BUDGET)"; \
	if [ "$$bytes" -gt "$(SIZE_BUDGET)" ]; then \
		echo "ocicfs binary exceeds the device budget"; \
		exit 1; \
	fi

# install places the binary under $(DESTDIR)$(PREFIX)/bin.
.PHONY: install
install: build
	install -D -m 0755 $(BIN)/$(CMD) $(DESTDIR)$(PREFIX)/bin/$(CMD)

# test runs the unit test suite.
.PHONY: test
test:
	$(GO) test $(GOFLAGS) ./...

# vet runs go vet across all packages.
.PHONY: vet
vet:
	$(GO) vet $(GOFLAGS) ./...

# lint enforces formatting (gofmt + gofumpt) and runs golangci-lint. CI runs
# this target, so it is the gate that rejects unformatted or unclean code.
.PHONY: lint
lint:
	$(GOLANGCI_LINT) fmt --diff
	$(GOLANGCI_LINT) run

# fmt applies the configured formatters (gofmt + gofumpt) in place.
.PHONY: fmt
fmt:
	$(GOLANGCI_LINT) fmt

# smoke runs the pull --from-dir -> ls -> inspect -> mount -> unmount -> remove
# lifecycle against a real image, for a quick check of the implemented
# functionality. Run it inside the privileged dev container (fs-verity +
# mkcomposefs), e.g.:
#   ./devshell.sh                                              # then: make smoke
#   docker run --rm --privileged -v "$PWD":/work ocicfs-dev make smoke
.PHONY: smoke
smoke: build
	./scripts/smoke.sh

# --- Debian packaging ---------------------------------------------------
#
# The .deb is built by debuild inside a local container image (debian/
# Dockerfile) so no Debian toolchain is needed on the host. The Debian
# version comes from the release tag (v0.1.0 -> 0.1.0, v0.1.0-rc.1 ->
# 0.1.0~rc.1 so finals sort after candidates); an untagged checkout gets a
# never-releasable snapshot version instead.

DEB_IMAGE ?= ocicfs-debuild:trixie
DEB_OUT ?= $(BIN)/deb

RELEASE_TAG := $(shell git describe --tags --exact-match --match 'v*' 2>/dev/null)
ifeq ($(RELEASE_TAG),)
DEB_VERSION ?= 0.0.0~snap.g$(shell git rev-parse --short HEAD 2>/dev/null || echo none)
else
DEB_VERSION ?= $(subst -rc.,~rc.,$(patsubst v%,%,$(RELEASE_TAG)))
endif

# deb-image builds the packaging container image.
.PHONY: deb-image
deb-image:
	docker build -t $(DEB_IMAGE) debian

# deb builds the .deb (plus .changes/.buildinfo) into $(DEB_OUT). VERSION,
# REVISION and DEB_VERSION are passed in explicitly: inside the container the
# git-derived defaults would silently fall back to "dev".
.PHONY: deb
deb: deb-image
	mkdir -p $(DEB_OUT)
	docker run --rm \
		-v "$(CURDIR)":/src:ro \
		-v "$(CURDIR)/$(DEB_OUT)":/out \
		-e VERSION=$(VERSION) -e REVISION=$(REVISION) -e DEB_VERSION=$(DEB_VERSION) \
		$(DEB_IMAGE) /src/debian/build-in-docker.sh

# deb-lint runs lintian over the built package.
.PHONY: deb-lint
deb-lint:
	docker run --rm -u "$$(id -u):$$(id -g)" \
		-v "$(CURDIR)/$(DEB_OUT)":/out:ro $(DEB_IMAGE) \
		sh -c 'lintian -I /out/*.changes'

# deb-test install-tests the .deb on clean Debian and Ubuntu containers:
# control fields, install, stamped version, reinstall, remove, purge.
.PHONY: deb-test
deb-test:
	DEB_VERSION=$(DEB_VERSION) VERSION=$(VERSION) ./scripts/deb-test.sh $(DEB_OUT)

# clean removes build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN)
