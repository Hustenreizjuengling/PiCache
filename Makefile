# PiCache build targets (GNU make, POSIX shell).
#
#   make            web UI + binary for this machine (bin/picache)
#   make build-all  static release binaries for linux/amd64, arm64, armv7
#   make dist VERSION=vX.Y.Z
#                   web UI + the release assets in dist/ (what the release
#                   workflow publishes; needs git and GNU tar)
#   make docker     container image (deploy/docker/Dockerfile)
#
# Override the version information with e.g. `make VERSION=v0.1.0`.

MODULE  := github.com/hustenreizjuengling/picache
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE   ?= picache

GO      ?= go
TAR     ?= tar
# Recursive (=) so that `dist` can use its own DATE.
LDFLAGS  = -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)
GOBUILD  = CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)"
GODIRS  := cmd internal

# $(call cross-build,DIR): the static Linux binaries in DIR.
define cross-build
GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(1)/picache-linux-amd64 ./cmd/picache
GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(1)/picache-linux-arm64 ./cmd/picache
GOOS=linux GOARCH=arm GOARM=7 $(GOBUILD) -o $(1)/picache-linux-armv7 ./cmd/picache
endef

# Release assets (docs/ARCHITECTURE.md, section 14.1).
DIST        := dist
DIST_ASSETS := get-picache.sh picache-deploy.tar.gz picache-linux-amd64 picache-linux-arm64 picache-linux-armv7
# SemVer 2.0 with a v prefix and an optional pre-release, without build
# metadata: v1.2.3, v1.2.3-rc.1.
SEMVER_ID   := (0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)
SEMVER_RE   := v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-$(SEMVER_ID)(\.$(SEMVER_ID))*)?
# Time of the last commit. `dist` uses it as the build date of the binaries
# and as the time stamp of every file in picache-deploy.tar.gz, so the same
# commit, Go version and web dependencies give the same files.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null)

.PHONY: all web build build-all dist test vet lint docker clean help

all: web build

help:
	@echo "targets: all (default) web build build-all dist test vet lint docker clean"

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
	$(call cross-build,bin)
	cp LICENSE THIRD_PARTY_NOTICES.md bin/

## dist: web UI, then dist/ with exactly the release assets and SHA256SUMS.
## VERSION must be a release version. picache-deploy.tar.gz holds the files
## of deploy/, LICENSE and THIRD_PARTY_NOTICES.md that git tracks (never
## untracked local files), sorted by name (git lists them byte-wise sorted),
## owned by 0:0, with the commit time. No pipes: POSIX sh has no pipefail.
dist: export PICACHE_DIST_VERSION = $(VERSION)
dist: DATE = $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)
dist:
	@v="$$PICACHE_DIST_VERSION"; \
	case "$$v" in ''|*[!0-9A-Za-z.-]*) ok=no ;; *) ok=yes ;; esac; \
	if [ "$$ok" = no ] || ! printf '%s\n' "$$v" | LC_ALL=C grep -Eqx '$(SEMVER_RE)'; then \
		echo "make dist: VERSION must be a release version such as v1.2.3 or v1.2.3-rc.1, not \"$$v\"" >&2; \
		exit 2; \
	fi
	@case '$(SOURCE_DATE_EPOCH)' in ''|*[!0-9]*) \
		echo "make dist: needs a git checkout (or SOURCE_DATE_EPOCH)" >&2; exit 2 ;; esac
	@$(TAR) --version 2>/dev/null | grep -q 'GNU tar' || { \
		echo "make dist: needs GNU tar (on macOS: brew install gnu-tar, then TAR=gtar)" >&2; exit 2; }
	@git ls-files --error-unmatch -- LICENSE THIRD_PARTY_NOTICES.md deploy scripts/get-picache.sh >/dev/null
	$(MAKE) --no-print-directory web
	rm -rf $(DIST)
	@mkdir -p $(DIST)
	$(call cross-build,$(DIST))
	git ls-files -z -- LICENSE THIRD_PARTY_NOTICES.md deploy > $(DIST)/deploy-files
	$(TAR) --create --file=$(DIST)/picache-deploy.tar --format=gnu \
		--owner=0 --group=0 --numeric-owner --mtime=@$(SOURCE_DATE_EPOCH) \
		--mode='u+rwX,go+rX,go-w' --no-recursion --null --files-from=$(DIST)/deploy-files
	rm $(DIST)/deploy-files
	gzip -9n $(DIST)/picache-deploy.tar
	cp scripts/get-picache.sh $(DIST)/get-picache.sh
	touch -d @$(SOURCE_DATE_EPOCH) $(DIST)/get-picache.sh
	cd $(DIST) && sha256sum $(DIST_ASSETS) > SHA256SUMS
	@cat $(DIST)/SHA256SUMS

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

## clean: remove binaries, release assets and the built UI (keeps the .gitkeep go:embed needs)
clean:
	rm -rf bin $(DIST)
	find internal/webui/dist -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +
