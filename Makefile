.PHONY: build test vet check

build:
	go build -o bin/promptd ./cmd/promptd

test:
	go test ./...

vet:
	go vet ./...

check: vet test build
