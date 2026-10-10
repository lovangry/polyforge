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
	# Skip-evidence note (polyforge#104, retired with polyforge#106): the
	# v1-harness legacy lane this note originally described is GONE — the
	# hatch, its guard and every legacy-lane skip died with the engine.
	# The note's mechanics still matter for the remaining gated lane (the
	# real-github ship-pr e2e skips outside short mode without gh):
	# go test suppresses skip messages unless -v (or -json) is on, so
	# plain `make test` green does NOT show them — verification evidence
	# that needs it must run `go test -v ./...` (or -json) explicitly.
	#
	# Explicit -timeout (rewind fix for the wi_QVLW3ceFGfSF verification
	# FAIL): the ./test acceptance package honestly runs ~11m on this
	# class of machine (646s measured: the w079 workspace-yaml suite alone
	# is ~73s across 28 daemon-driven scenarios, and the pre-existing v1/
	# v2/v3 acceptance suites carry the rest) — LONGER than go's default
	# 600s per-package timeout, so `go test ./...` with no -timeout panics
	# on wall clock while every test passes. The target passes an explicit
	# timeout with headroom instead of failing a green package; per-test
	# hangs still trip it long before 20m (the package's slowest single
	# test is ~62s).
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
