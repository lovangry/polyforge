.PHONY: build test lint check-xrefs

build:
	go build ./...

test:
	go test ./...

lint:
	go vet ./...

check-xrefs:
	python3 docs/design/polyforge-vnext/tools/check-xrefs.py
