GO ?= go
PYTHON ?= python3

.PHONY: build test test-go test-py vet fmt check

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o bin/gateway ./cmd/gateway

test: test-go test-py

test-go:
	$(GO) test -race ./...

test-py:
	$(PYTHON) -m unittest discover -s worker/tests -v

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

check: vet test
