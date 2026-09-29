.PHONY: build test lint check-xrefs check openapi-lint

build:
	go build ./...

test:
	go test ./...

lint:
	go vet ./...

check-xrefs:
	python3 docs/design/polyforge-vnext/tools/check-xrefs.py

# Lint/consistency check for the frozen wire contract: verifies the
# operation set in api/openapi.yaml against the canonical endpoint inventory
# of 02-aihub-core.md §5.1 (26 business rows + M1–M5 + row-26 dependency
# routes) plus structural rules (single server, Bearer scheme, canonical-row
# citations, generation-token applicability, $ref resolution, error-code
# status map). The tool carries the `ignore` build tag and is out of the
# normal build path.
openapi-lint:
	GOWORK=off go run ./api/openapi-lint.go

# Full gate: build + tests + vet + doc cross-refs + wire-contract lint.
check: build test lint check-xrefs openapi-lint
