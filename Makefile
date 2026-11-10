.PHONY: build check-config fmt run test

build:
	go build -o healthz ./cmd/healthz

check-config:
	go run ./cmd/healthz -config config.json -check-config

fmt:
	gofmt -w cmd internal

run:
	go run ./cmd/healthz -config config.json

test:
	go test ./...
