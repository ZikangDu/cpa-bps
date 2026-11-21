GO ?= go
LIB_EXT := $(if $(filter darwin,$(shell $(GO) env GOOS)),dylib,so)
PLUGIN := build/cpa-bps.$(LIB_EXT)

.PHONY: build test

build:
	mkdir -p build
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -o $(PLUGIN) ./cmd/cpa-bps
	$(GO) build -trimpath -o build/bps-document-worker ./cmd/bps-document-worker

test:
	$(GO) test -race -count=1 ./...
