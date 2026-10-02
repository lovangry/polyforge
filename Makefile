.PHONY: check build vet fmt test

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./... -count=1

fmt:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; \
	fi

check: build vet fmt test
