# syntax=docker/dockerfile:1
#
# PiCache container image. Build from the repository root:
#
#   docker build -f deploy/docker/Dockerfile \
#     --build-arg VERSION=v0.1.0 --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) -t picache .
#
# Multi-arch (no emulation needed, Go cross-compiles):
#   docker buildx build -f deploy/docker/Dockerfile \
#     --platform linux/amd64,linux/arm64,linux/arm/v7 -t picache .
#
# The image starts as root so it can bind :53/:80/:443 with host networking
# (Docker grants no ambient capabilities to non-root users), then drops to
# PICACHE_RUN_AS (65532:65532) in-process before it touches any file.
# Use deploy/docker/docker-compose.yml, which drops all other capabilities.

# ---- web UI (built once on the build host, architecture independent) ----
FROM --platform=$BUILDPLATFORM node:25-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# vite writes the bundle to ../internal/webui/dist (= /src/internal/webui/dist).
RUN npm run build

# ---- Go binary (static, cross-compiled for the target platform) ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/internal/webui/dist/ internal/webui/dist/
# Builds without build args (docker compose) still get a build date, which
# the clock guard needs.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    if [ "$TARGETARCH" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    if [ "$DATE" = unknown ]; then DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ); fi; \
    pkg=github.com/hustenreizjuengling/picache/internal/version; \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build \
      -ldflags "-s -w -X $pkg.Version=$VERSION -X $pkg.Commit=$COMMIT -X $pkg.Date=$DATE" \
      -o /out/picache ./cmd/picache
# Volume mount points, owned by the runtime user so named volumes inherit
# owner and mode (PiCache never chowns). They are copied via their parent
# directory because COPY of a directory itself does not keep its mode.
RUN install -d -m 0750 /out/state/data /out/state/cache && install -d -m 0755 /out/srv/picache
# License texts that go with every copy, for /usr/share/doc/picache (root,
# world-readable). Modes are set here because COPY --chmod would also apply
# the file mode to the directory it creates.
COPY LICENSE THIRD_PARTY_NOTICES.md /tmp/doc/
RUN install -d -m 0755 /out/doc/picache && install -m 0644 /tmp/doc/* /out/doc/picache/

# ---- runtime: distroless static (CA certificates, tzdata, no shell) ----
FROM gcr.io/distroless/static-debian13
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
LABEL org.opencontainers.image.title="PiCache" \
      org.opencontainers.image.description="Filtering DNS server and LanCache-compatible download cache with a web UI" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/hustenreizjuengling/picache" \
      org.opencontainers.image.documentation="https://github.com/hustenreizjuengling/picache/blob/main/docs/DEPLOYMENT.md" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT" \
      org.opencontainers.image.created="$DATE"
COPY --from=build /out/picache /picache
COPY --from=build --chown=65532:65532 /out/state/ /
COPY --from=build /out/srv/ /srv/
COPY --from=build /out/doc/ /usr/share/doc/
ENV PICACHE_DATA_DIR=/data \
    PICACHE_CACHE_DIR=/cache \
    PICACHE_RUN_AS=65532:65532 \
    PICACHE_MOUNT_ROOT=/srv/picache
VOLUME ["/data", "/cache"]
# DNS, LanCache HTTP, SNI pass-through, web UI (HTTP, HTTPS).
EXPOSE 53/udp 53/tcp 80/tcp 443/tcp 8080/tcp 8443/tcp
# Root only until the listeners are bound; see the header.
USER 0:0
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD ["/picache", "healthcheck"]
ENTRYPOINT ["/picache"]
CMD ["serve"]
