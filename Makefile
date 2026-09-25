# PiCache build targets (GNU make, POSIX shell).
#
#   make            web UI + binary for this machine (bin/picache)
#   make build-all  static release binaries for linux/amd64, arm64, armv7
#   make docker     container image (deploy/docker/Dockerfile)
#
# Override the version information with e.g. `make VERSION=v0.1.0`.

MODULE  := github.com/hustenreizjuengling/picache
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE   ?= picache

GO      ?= go
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)
GOBUILD := CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)"
GODIRS  := cmd internal

.PHONY: all web build build-all test vet lint docker clean help

all: web build

help:
	@echo "targets: all (default) web build build-all test vet lint docker clean"

## web: build the Svelte UI into internal/webui/dist (embedded by the binary)
web:
	cd web && npm ci && npm run build

## build: binary for the host platform (embeds whatever internal/webui/dist holds)
build:
	@mkdir -p bin
	$(GOBUILD) -o bin/picache ./cmd/picache

## build-all: static Linux release binaries (run `make web` first to embed the UI)
build-all:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o bin/picache-linux-amd64 ./cmd/picache
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o bin/picache-linux-arm64 ./cmd/picache
	GOOS=linux GOARCH=arm GOARM=7 $(GOBUILD) -o bin/picache-linux-armv7 ./cmd/picache
	cp LICENSE THIRD_PARTY_NOTICES.md bin/

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	GOOS=linux GOARCH=amd64 $(GO) vet ./...

## lint: fail if any Go file is not gofmt-formatted
lint:
	@out=$$(gofmt -l $(GODIRS)); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

docker:
	docker build -f deploy/docker/Dockerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## clean: remove binaries and the built UI (keeps the .gitkeep go:embed needs)
clean:
	rm -rf bin
	find internal/webui/dist -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +
