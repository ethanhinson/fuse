.PHONY: build install test lint

build:
	go build -o fuse ./cmd/fuse

install:
	go install ./cmd/fuse

test:
	go test ./...

lint:
	go vet ./...
