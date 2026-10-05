-include .env
export

.PHONY: dev build test lint

dev:
	go run .

build:
	go build -o bin/alumni .

test:
	go test ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "run gofmt -w ."; exit 1)
	go vet ./...
