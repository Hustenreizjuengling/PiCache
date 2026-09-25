# Contributing to PiCache

Thank you for your interest in PiCache. The project's priorities are
**security, simplicity, correctness, performance, features**, in that order
([docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)). Please open an issue to
discuss a larger change or a new dependency before you start working on it.

**Security issues** do not belong in public issues or pull requests. Report
them privately as described in [docs/SECURITY.md](docs/SECURITY.md).

## Prerequisites

- Go 1.27
- Node.js 22 with npm (web UI)
- GNU make and a POSIX shell
- Optional: Docker (container image, installer smoke test) and ShellCheck
  (the CI lints the shell scripts with it)

## Build, test and lint

The `Makefile` has these targets:

```sh
make              # web UI + bin/picache for this machine (same as `make all`)
make web          # npm ci + vite build into internal/webui/dist
make build        # bin/picache; embeds whatever internal/webui/dist holds
make build-all    # static bin/picache-linux-{amd64,arm64,armv7} (run `make web` first)
make test         # go test ./...
make vet          # go vet ./... for this platform and for linux/amd64
make lint         # fails if a Go file under cmd/ or internal/ is not gofmt-formatted
make docker       # container image from deploy/docker/Dockerfile
make clean        # remove bin/ and the built UI
```

The CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) also runs these
checks. You can run them before you push:

```sh
cd web && npm ci && npm run check            # svelte-check: 0 errors, 0 warnings
GOOS=linux GOARCH=arm GOARM=7 go vet ./...   # 32-bit ARM
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
shellcheck -s sh deploy/install.sh scripts/test-install.sh
sh scripts/test-install.sh bin/picache-linux-amd64   # installer smoke test (Docker)
```

The Go tests run on Linux, Windows and macOS in the CI. They need no network
access.

## Running PiCache locally

Run the backend on unprivileged ports with a local data directory:

```sh
make build
bin/picache serve --dev --data-dir ./data --cache-dir ./cache \
  --dns-listen 127.0.0.1:1053 --cache-listen off --sni-listen off \
  --web-listen 127.0.0.1:8080 --web-tls-listen off
```

`./data` and `./cache` are ignored by git. Get the setup token with
`PICACHE_DATA_DIR=./data bin/picache setup-token`.

For work on the UI, start the Vite development server next to it. It proxies
`/api` to `127.0.0.1:8080` and reloads on changes:

```sh
cd web
npm ci
npm run dev
```

A binary built without `make web` serves a short "web UI is not built"
notice instead of the UI; the API still works. To check a production build
without replacing the embedded `internal/webui/dist`, run
`npx vite build --outDir <temporary directory> --emptyOutDir` in `web/`.

## Conventions

The binding rules are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
(sections 4 and 5), [docs/DESIGN.md](docs/DESIGN.md) and
[web/README.md](web/README.md). The most important ones:

**Go**

- Code is `gofmt`-formatted. Logging uses `log/slog` with a `component`
  attribute and never logs secrets, passwords, tokens or CDN query strings.
- Third-party modules are limited to the list in ARCHITECTURE section 4.
  Adding a module needs an explicit decision, so ask in an issue first.
- Keep the package dependency rules of section 4: domain packages never
  import `api`, and `dnsserver`, `proxy` and `sni` use consumer-side
  interfaces for their collaborators.
- OS-specific code goes into `*_linux.go` files with portable `*_other.go`
  fallbacks, so `go build ./...` and `go test ./...` work on every OS.
- Tests are table-driven unit tests per package, with `testing/synctest` for
  time and `httptest` for HTTP, and without network access.
- SQL uses parameterised statements only. Table names start with their
  component (`dns_records`, `filter_lists`, …), and each component migrates
  only its own tables with append-only steps.
- Every cache, map, queue, list and upload has an explicit size limit.
- A change to the REST API updates [docs/API.md](docs/API.md) in the same
  pull request; the same applies to ARCHITECTURE.md and DEPLOYMENT.md.

**Web UI**

- No new npm dependencies without a discussion first.
- Never render HTML from data (no `{@html}`, no `innerHTML`), no inline
  scripts, no `eval`, no requests to other origins. The Content-Security-Policy
  enforces most of this.
- Every string exists in English and German (`web/src/i18n/{en,de}`).
  German uses "du". Sentence case everywhere.
- Pages work at 360 px width, by keyboard, in light and dark mode and with
  `prefers-reduced-motion`.

**Documentation and test data**

- Plain, precise English in sentence case, without marketing language or
  emoji.
- Use example data: documentation address ranges (`192.0.2.0/24`,
  `198.51.100.0/24`, `203.0.113.0/24`, `2001:db8::/32`) or generic private
  ones, and names below `example.com` or `.test`. Never commit real
  passwords, tokens, personal data or the addresses of your own network.

## Commits and pull requests

- One logical change per commit. The subject is a short imperative sentence,
  optionally prefixed with the area it touches, for example
  `dns: answer server names with the host address in bridge mode` or
  `install.sh: refuse to run the service as a login account`.
- The body explains why the change is needed and what it changes.
- Fixes come with a regression test where possible.
- User-visible changes get a line under "Unreleased" in
  [CHANGELOG.md](CHANGELOG.md).
- Pull requests need a green CI. The pull request template lists the checks.

By contributing, you agree that your contributions are licensed under the
[MIT license](LICENSE) of this project.
