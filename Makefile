# SPDX-License-Identifier: Apache-2.0

GO ?= go
GOFLAGS ?=
GOLANGCI_LINT ?= golangci-lint

# Build artifacts live under bin/.
BIN := bin
CMD := knarr
PKG := github.com/foundriesio/knarr

# Install location (overridable, e.g. DESTDIR for packaging).
PREFIX ?= /usr/local
DESTDIR ?=

# Device-footprint budget for the knarr binary, in bytes. The total device
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
all: lint vet test build

# build compiles the knarr CLI into bin/ as a stripped, trimmed binary.
.PHONY: build
build:
	$(GO) build $(GOFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$(CMD) ./cmd/$(CMD)

# size builds the stripped binary and checks it against the device budget so the
# footprint is tracked with data rather than estimates.
.PHONY: size
size: build
	@bytes=$$(wc -c < $(BIN)/$(CMD)); \
	printf 'knarr binary: %s bytes (%s MiB); budget %s bytes\n' \
		"$$bytes" "$$((bytes / 1024 / 1024))" "$(SIZE_BUDGET)"; \
	if [ "$$bytes" -gt "$(SIZE_BUDGET)" ]; then \
		echo "knarr binary exceeds the device budget"; \
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

# smoke runs the pull --from-dir -> mount -> unmount happy path against a real image,
# for a quick by-hand check of the implemented functionality. Run it inside the
# privileged dev container (fs-verity + mkcomposefs + mount.composefs), e.g.:
#   ./devshell.sh                                              # then: make smoke
#   docker run --rm --privileged -v "$PWD":/work knarr-dev make smoke
.PHONY: smoke
smoke: build
	./scripts/smoke.sh

# clean removes build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN)
