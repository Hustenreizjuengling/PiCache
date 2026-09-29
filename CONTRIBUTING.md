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
- GNU make and a POSIX shell (`make dist` also needs git, GNU tar and
  `dpkg-deb` from the package `dpkg`, for the Debian packages; it stops with
  "make dist: needs dpkg-deb (package dpkg)" without it)
- Optional: Docker (container image, the installer and package tests) and
  ShellCheck (the CI lints the shell scripts with it)

## Build, test and lint

The `Makefile` has these targets:

```sh
make              # web UI + bin/picache for this machine (same as `make all`)
make web          # npm ci + vite build into internal/webui/dist
make build        # bin/picache; embeds whatever internal/webui/dist holds
make build-all    # static bin/picache-linux-{386,amd64,arm64,armv6,armv7,riscv64} (run `make web` first)
make dist VERSION=v1.2.3   # web UI + the release files in dist/, Debian packages included (see Releases)
make test         # go test ./...
make vet          # go vet ./... for this platform, linux/amd64, linux/386, linux/riscv64 and linux/arm (GOARM=6)
make lint         # fails if a Go file under cmd/ or internal/ is not gofmt-formatted
make docker       # container image from deploy/docker/Dockerfile
make clean        # remove bin/, dist/ and the built UI
```

The CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) also runs these
checks. You can run them before you push:

```sh
cd web && npm ci && npm run check            # svelte-check (0 errors, 0 warnings), the translation and search-index checks
cd web && npm run build                      # also checks the bundle sizes
GOOS=linux GOARCH=arm GOARM=7 go vet ./...   # 32-bit ARM
GOARCH=386 go test ./...                     # 32-bit (on Linux or Windows amd64)
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
shellcheck -s sh deploy/install.sh scripts/*.sh
shellcheck -s sh -e SC2154 -e SC2034 deploy/debian/*.sh   # fragments of the maintainer scripts
sh scripts/test-get-picache.sh                             # get-picache.sh (no Docker)
sh scripts/test-install.sh bin/picache-linux-amd64 debian:13   # installer test (Docker; also fedora:latest, archlinux:latest, opensuse/tumbleweed:latest)
make dist VERSION=v0.0.0-dev && sh scripts/check-dist.sh dist v0.0.0-dev && sh scripts/test-deb.sh dist debian:13
```

`scripts/test-deb.sh` is needed whenever `deploy/debian`, `deploy/systemd` or
`deploy/install.sh` change: the package's maintainer scripts are
`install.sh` without its first and last line plus `deploy/debian/<name>.sh`.
So `install.sh` keeps its constants and functions at the top level, does
everything else in `main`, and ends with exactly `main "$@"`.

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
- Migration steps are append-only **from the moment they are merged to
  `main`**, not from the release: the nightly build may already have applied
  a merged step on test machines. Fix a merged step with a new step, never by
  editing it.
- Every cache, map, queue, list and upload has an explicit size limit.
- A change to the REST API updates [docs/API.md](docs/API.md) in the same
  pull request; the same applies to ARCHITECTURE.md and DEPLOYMENT.md.

**Web UI**

- No new npm dependencies without a discussion first.
- Never render HTML from data (no `{@html}`, no `innerHTML`), no inline
  scripts, no `eval`, no requests to other origins. The Content-Security-Policy
  enforces most of this.
- **Every string exists in every language** (`web/src/i18n/<id>/`):
  a change that adds or changes a UI text translates it into every language
  in the same pull request (the types and `npm run check` refuse missing
  keys, plural forms, placeholders or changed commands). The glossary and
  the rules are in [docs/TRANSLATING.md](docs/TRANSLATING.md);
  German uses "du". Sentence case everywhere. Maintainers review the
  code-like parts (commands, paths, options) of every translation.
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

## Releases

Maintainers publish releases by pushing a tag. Versions follow
[SemVer](https://semver.org/) with a `v` prefix: `vX.Y.Z`, or `vX.Y.Z-rc.N`
for a pre-release. Every tag with a hyphen becomes a GitHub pre-release, which
PiCache offers only to installations with **Include pre-releases** on. The
rules for the release files are in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#14-releases-and-updates)
(section 14).

### Publishing a release

1. Make sure `main` is green and contains everything for the release.
2. In `CHANGELOG.md`, move the entries under `## [Unreleased]` to a new
   section `## [X.Y.Z] - YYYY-MM-DD` (the release date, no `v`), leave an
   empty `## [Unreleased]` above it, and update the links at the end:

   ```markdown
   ## [Unreleased]

   ## [1.2.0] - 2026-10-05

   ### Added

   - …

   [Unreleased]: https://github.com/Hustenreizjuengling/PiCache/compare/v1.2.0...HEAD
   [1.2.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v1.1.0...v1.2.0
   ```

   This section becomes the release notes on GitHub and in the web UI. Updates
   replace only the binary: if the release changes files in `deploy/` (units,
   installer), start the section with a note that `install.sh` from
   `picache-deploy.tar.gz` must be run after updating. A pre-release needs a
   section of its own, for example `## [1.2.0-rc.1] - 2026-10-01`.
3. Check the notes with `sh scripts/release-notes.sh v1.2.0`.
4. Commit (`Release v1.2.0`), push to `main` and wait for the CI.
5. Tag that commit and push the tag:

   ```sh
   git tag -a v1.2.0 -m "PiCache v1.2.0"
   git push origin v1.2.0
   ```

The release workflow ([.github/workflows/release.yml](.github/workflows/release.yml))
then:

- checks the tag and takes the release notes from `CHANGELOG.md`; it stops
  if the section is missing or empty;
- runs `make dist VERSION=<tag>`: the web UI, the six static binaries, the
  five Debian packages, `picache-deploy.tar.gz` and `SHA256SUMS` in `dist/`,
  then `scripts/check-dist.sh`;
- signs `SHA256SUMS` in a separate job (environment `release`, no checkout,
  no third-party actions) with the secret `RELEASE_SIGNING_KEY` into
  `SHA256SUMS.sig`, verifies the signature against `docs/release-key.pem`,
  and checks the checksums and that the amd64 binary reports the tag;
- creates the GitHub release with exactly these files (the list of
  `BINARIES` and `DEB_ARCHES` of the Makefile, repeated in the workflow's job
  env, plus `get-picache.sh`, `picache-deploy.tar.gz`, `SHA256SUMS` and
  `SHA256SUMS.sig`), as a pre-release for tags with a hyphen;
- then builds and pushes the image `ghcr.io/hustenreizjuengling/picache` for
  linux/amd64, linux/arm64, linux/arm/v7 and linux/riscv64 (the platforms of
  the distroless base image; it has no arm/v6 and no 386), tagged `X.Y.Z` and,
  for stable releases, also `X.Y` and `latest`;
- and, when Docker Hub is set up (below), copies that image by digest to
  Docker Hub with the same tags.

A new architecture is added in one change to the Makefile (`BINARIES`,
`DEB_ARCHES`, `cross-build`), both workflows' env, `scripts/get-picache.sh`
(`arch`), `scripts/build-deb.sh` and `update.AssetName`/`update.DebianArch`;
`internal/update/assets_test.go` checks that they agree.

Nothing is published when a step before the release fails. If the workflow
fails, fix the cause, then either re-run the failed jobs (for a temporary
failure, for example of the image job), or delete the tag
(`git push --delete origin v1.2.0 && git tag -d v1.2.0`) and a draft release
it may have left, and tag the fixed commit again. Never move the tag of a
published release, because installations may already run it: publish a new
patch version instead.

`make dist VERSION=v1.2.0` builds the same files locally (it needs git, GNU
tar and `dpkg-deb`; on macOS install `gnu-tar` and pass `TAR=gtar`). It
refuses versions that are not SemVer with a `v` prefix or whose pre-release
part contains a hyphen (`v1.2.3-beta-2`: Debian would sort such a version
differently, `scripts/deb-version.sh`), and it packs only files that git
tracks. The `Maintainer` of the packages is the Makefile variable
`DEB_MAINTAINER` (default `PiCache <picache@invalid>`, a placeholder until a
contact address is published). Lintian cleanliness is not a goal.

### One-time setup: the signing key

Releases are signed with an Ed25519 key. Its public half is
`docs/release-key.pem`, and the same key is compiled into PiCache
(`internal/update/keys.go`). The private half is used only by the release
workflow and is never committed.

A new key (only for a new project or a key rotation, see
[docs/SECURITY.md](docs/SECURITY.md#the-release-key)) is created on a trusted
machine with OpenSSL 3:

```sh
(umask 077 && openssl genpkey -algorithm ed25519 -out picache-release-key.pem)
openssl pkey -in picache-release-key.pem -pubout -out docs/release-key.pem
openssl pkey -in picache-release-key.pem -pubout -outform DER | tail -c 32 | base64   # for keys.go
```

The repository owner stores it for the workflow:

1. **Settings → Environments → New environment** `release` with
   **Deployment branches and tags → Selected branches and tags** and a tag
   rule `v*` (optionally a required reviewer, so every release waits for
   your approval). Then add the environment secret `RELEASE_SIGNING_KEY`
   there: the whole content of `picache-release-key.pem`, including the
   `BEGIN` and `END` lines. With the GitHub CLI:
   `gh secret set RELEASE_SIGNING_KEY --env release --repo Hustenreizjuengling/PiCache < picache-release-key.pem`.
   Do not keep a repository-wide secret of the same name.
2. Keep an offline backup of the file (for example on an encrypted medium)
   and delete other copies. If the key is lost, a planned key rotation is no
   longer possible: installed versions could then only be updated by hand.

The workflow stops with an error if the secret is missing, and it verifies
the signature against `docs/release-key.pem` before it publishes anything, so
a wrong key cannot produce a release.

After the first release, open the package
`ghcr.io/hustenreizjuengling/picache` on GitHub (**Packages**) and set its
visibility to public (**Package settings → Change visibility**): GitHub
creates new container packages as private, even for a public repository.

### Optional: the Docker Hub mirror

The job `dockerhub` of the release workflow copies the released image from
GHCR to Docker Hub by digest (never a rebuild) with the same tags. It is
skipped while the secret `DOCKERHUB_TOKEN` is missing. To set it up:

1. On Docker Hub, create the repository (for example `<namespace>/picache`)
   and a **repository-scoped access token** with read and write access to
   it (never the account password).
2. On GitHub, **Settings → Environments → New environment** `dockerhub` with
   **Deployment branches and tags → Selected branches and tags** and a tag
   rule `v*`. Add the environment secrets `DOCKERHUB_USERNAME` (the Docker
   Hub user) and `DOCKERHUB_TOKEN` (the access token).
3. Add the repository variable **Settings → Secrets and variables → Actions →
   Variables** `DOCKERHUB_IMAGE` = `<namespace>/picache`.

The job reads the images from GHCR with `packages: read` only and runs no
checked-out code.

### Nightly builds and the nightly key

`.github/workflows/nightly.yml` publishes a nightly pre-release of `main`
every day that `main` changed (and on demand, **Actions → Nightly → Run
workflow**), named `v<next patch>-nightly.<YYYYMMDD>.<n>`, and deletes
nightlies beyond the newest 7. It signs with a separate Ed25519 key: its
public half is `docs/nightly-key.pem` and the list `nightlyKeys` in
`internal/update/keys.go`, which verifies only versions containing
`-nightly.` (the release key never signs them, the nightly key never signs a
release). Create it like the release key:

```sh
(umask 077 && openssl genpkey -algorithm ed25519 -out picache-nightly-key.pem)
openssl pkey -in picache-nightly-key.pem -pubout -out docs/nightly-key.pem
openssl pkey -in picache-nightly-key.pem -pubout -outform DER | tail -c 32 | base64   # for nightlyKeys
```

Then create the environment **`nightly`** with **Deployment branches and
tags → Selected branches** and the rule `main`, and add the environment
secret `NIGHTLY_SIGNING_KEY` (the whole PEM file):
`gh secret set NIGHTLY_SIGNING_KEY --env nightly --repo Hustenreizjuengling/PiCache < picache-nightly-key.pem`.
The workflow verifies every signature against `docs/nightly-key.pem` before
it publishes. Nightlies push no container images (a scheduled job would need
`packages: write`, with which it could overwrite `latest`; build `main`
locally with `docker compose up -d --build` instead), and hosts install them
only with `install.sh --nightly`. Nightlies carry Debian packages too.
