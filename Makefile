.PHONY: build test vet check release

VERSION ?= v0.0.0-dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD)
SOURCE_DATE_EPOCH ?= $(shell git show -s --format=%ct HEAD)

build:
	go build -o bin/promptd ./cmd/promptd

test:
	go test ./...

vet:
	go vet ./...

check: vet test build

release:
	SOURCE_DATE_EPOCH="$(SOURCE_DATE_EPOCH)" go run ./cmd/package-release --version "$(VERSION)" --commit "$(COMMIT)" --output dist
