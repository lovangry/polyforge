# Version stamped into both binaries at link time: internal/version.Version,
# printed by `pf --version` / `aihub --version`. Defaults to the git tag when
# HEAD is exactly tagged, else "dev". The documented release procedure tags
# AFTER the build, so it passes VERSION=<tag> explicitly to `make release`.
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || echo dev)
GO_LDFLAGS := -X github.com/silgrid/polyforge/internal/version.Version=$(VERSION)

.PHONY: check build vet fmt test release

build:
	go build -ldflags "$(GO_LDFLAGS)" ./...

vet:
	go vet ./...

test:
	# go test suppresses skip messages unless -v (or -json) is on, so a plain
	# `make test` does NOT show which tests skipped (e.g. the real-github ship-pr
	# e2e outside short mode without gh); run `go test -v ./...` when that
	# evidence matters.
	# Explicit -timeout: the ./test acceptance package runs longer than go's
	# default 10m per-package timeout, which would panic on wall clock while
	# every test passes on content. 20m relaxes only that per-package watchdog.
	go test ./... -count=1 -timeout 20m

fmt:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; \
	fi

check: build vet fmt test

# release builds the two shipped binaries with the git tag injected into
# internal/version.Version via -ldflags -X, into artifacts/release-<tag>/.
# Pass VERSION=<tag> (the release procedure builds before tagging) or run
# it on the tagged commit itself. Refuses to stamp the un-injected "dev".
release:
	@if [ "$(VERSION)" = dev ]; then \
		echo "make release: no git tag on HEAD and no VERSION=<tag> given; refusing to stamp 'dev'"; \
		exit 1; \
	fi
	GOWORK=off go build -ldflags "$(GO_LDFLAGS)" -o artifacts/release-$(VERSION)/ ./cmd/aihub ./cmd/pf
	@echo "release: injected internal/version.Version=$(VERSION) (ldflags -X) into aihub and pf"
