.PHONY: check build vet test

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./... -count=1

check: build vet test
