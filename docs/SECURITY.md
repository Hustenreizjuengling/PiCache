# Security

PiCache is a DNS resolver, an HTTP cache and a web application on your local
network. Its priorities are **security, simplicity, correctness,
performance**, in that order. This page summarises the threat model
(the binding details are in [ARCHITECTURE.md §6](ARCHITECTURE.md#6-security-model)),
gives a hardening checklist and explains how to report vulnerabilities.

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub's private
vulnerability reporting:
[report a vulnerability](https://github.com/Hustenreizjuengling/PiCache/security/advisories/new)
(or **Security → Report a vulnerability** on the repository page). Do not
open a public issue, discussion or pull request for a vulnerability. If
private reporting is not available, open an issue that only asks for a
private contact, without any details.

Include the version (`picache version`, or the commit you built), the
deployment type, and steps to reproduce. Never include real passwords,
tokens, setup tokens or NAS credentials, and do not attach databases or
backups: they contain secrets and personal data (query logs).

Security fixes are published as a new release (see [Updates](#updates)) and
listed in [CHANGELOG.md](../CHANGELOG.md). Only the newest release gets
fixes; please test against it or a current build of `main`.

## Scope and assumptions

- PiCache serves a **trusted LAN**. Clients on the LAN can use the DNS
  resolver and the cache; that is their purpose. The web UI requires an
  account.
- Out of scope: an attacker with root on the host, and a compromised PiCache
  process. The process holds the master key, so encrypting stored secrets at
  rest protects database copies and backups, not a running compromise.
- PiCache must never be reachable from the Internet. Do not forward its ports.

## Threat model summary

| Threat | Controls |
|---|---|
| Open resolver, DNS amplification | Answers only loopback, private (RFC 1918, ULA, CGNAT, link-local) and directly connected private networks, plus CIDRs you add. Other UDP queries are dropped; TCP connections are closed at accept. Per-client rate limit (default 50 qps, burst 200), `ANY` refused, CHAOS/`version.bind` refused, EDNS capped at 1232 bytes. `allowAllNetworks` is an explicit, dangerous switch. |
| DNS cache poisoning | Encrypted upstreams by default (DNS-over-HTTPS; DNS-over-TLS is also supported), random IDs and ports, the question is verified on every reply, in-flight deduplication, no client EDNS options forwarded. |
| Private reverse-DNS leaks | PTR/SOA/NS queries for private and special-use reverse zones never reach public upstreams. |
| Open HTTP proxy / SSRF via :80 | Only hosts of known cache services are served; unknown hosts get 403. Upstream addresses must be public unicast and not this machine; link-local (cloud metadata) is always refused, and redirects are re-checked. Non-canonical paths are never stored. Per-client fill limits and at most 16 ranges per request prevent WAN amplification. |
| Open TLS relay via :443 | TLS is never terminated. The SNI must match an enabled service, the same SSRF rules apply, and connections are capped and time out. |
| Hostile blocklists, cache-domains data or NAS content | Sizes, counts, names and patterns are validated; public-suffix patterns are rejected. Files are accessed through `os.Root`. Slice files carry validated, CRC-checked headers. |
| Web UI takeover on first start | One-time setup token (logged and stored with mode 0600, compared in constant time, first user created atomically), or provisioning from `PICACHE_ADMIN_PASSWORD_FILE`. After setup, `/auth/setup` answers 403 at once, uses no share of the global attempt limit and counts as a failed attempt of the caller. |
| Password guessing | argon2id; per client 5 failures → 15 min lockout; per user name a delay from the 5th failure (1 s, doubling, at most 30 s, reset by a successful sign-in), never a lockout; TOTP failures count; a global attempt limit; optional TOTP with single-use codes. Every sign-in gives the browser a device cookie (sealed with the master key, `HttpOnly`, 180 days, not a credential): a browser that signed in before is throttled by its own device key (5 failures → 15 min) instead of the user-name delay, so failed attempts from other LAN hosts cannot keep it out, and a copied device cookie allows no more guesses than one client. Password confirmations of signed-in users (tokens, TOTP, password change, restore) are throttled per client and per session (5 failures → 15 min each) and by the global limit, not by the user-name delay. |
| Session theft, CSRF | 256-bit session tokens stored hashed; cookie `HttpOnly`, `SameSite=Strict`; over HTTPS `Secure` and named `__Host-picache_session`, so a plain-HTTP origin on the same host cannot plant or overwrite it; `PICACHE_WEB_SECURE_COOKIES` sets `Secure` behind a TLS-terminating reverse proxy; idle and absolute timeouts; sessions re-checked on every request; cross-origin protection; JSON-only request bodies. A stolen session alone cannot create API tokens or enrol TOTP (both need the current password); changing the password ends all other sessions and all API tokens (unless kept explicitly); enabling TOTP ends all other sessions. |
| API token misuse | Tokens have scope `read` or `admin` and can never manage tokens, passwords, TOTP or sessions, nor restore a backup. Backups never contain accounts (users, password hashes, TOTP secrets, sessions, tokens), and a restore keeps the accounts and tokens of the running instance, so an admin token cannot become the interactive account. Live streams re-check the token every 15 s. |
| Malicious backup upload | A restore needs an interactive session and the current password. Uploads with triggers, views, virtual tables, generated columns, tables or indexes the running PiCache does not have, indexes defined differently from the running PiCache's (including named indexes disguised as automatic ones), or altered account tables are refused, at upload and again at the next start. The restore keeps the accounts, API tokens and audit log of the running instance and ends all sessions. Every database connection runs with `trusted_schema` off. PiCache creates no triggers or views: any found in `picache.db` (e.g. planted through a restore by an older version) are removed at start with a warning, by `picache reset-password`, and from every backup copy; revoking sessions or tokens and scrubbing a backup verify that the rows are really gone. |
| DNS rebinding against the UI | Host allowlist (IP addresses, localhost, this machine's names, configured hosts); other hosts get 421. |
| XSS, clickjacking | Strict Content-Security-Policy, `X-Frame-Options: DENY`, `nosniff`, `no-referrer`. The UI never renders HTML from data. |
| Secret leakage | NAS passwords, notification secrets and TOTP secrets are sealed (XChaCha20-Poly1305) with a master key that is never part of a backup. Secrets are write-only in the API and redacted in logs, snippets, notifications and the audit log. Only the root helper decrypts NAS passwords. CDN query strings are never logged or stored. |
| Outbound notifications | Only admins configure channels. PiCache sends only to the URLs they entered (http or https, no redirects, no proxy, verified TLS, 10 s), and never to link-local (cloud metadata), multicast or unspecified addresses. Private and loopback addresses are allowed on purpose. A stored secret is never sent to a changed server. Messages carry no secrets, passwords, tokens, session data or user names. Details in [Notifications](#notifications). |
| Scheduled backups | Same content as downloaded backups (no accounts; sealed secrets only on request), written only to the data directory or to a storage target's store, without following symbolic links. Details in [Scheduled backups](#scheduled-backups). |
| Network discovery scan | Admins only, audited, at most one per minute: one empty UDP datagram per address of this machine's private IPv4 subnets (at most 512, at most 200 per second) from an unprivileged socket; no raw sockets or capabilities. Details in [Parental controls and the network check](#parental-controls-and-the-network-check). |
| Privilege escalation | The service runs unprivileged and never holds `CAP_SYS_ADMIN`. NAS mounts are done by systemd on request of a separate root helper that re-validates every request and never trusts the database: it opens it read-only as a regular file (no links, FIFOs or devices) with an untrusted schema, touches only names derived from the target id, never follows links in the service-owned request directory, and runs sandboxed with a memory limit. |
| Resource exhaustion | Every cache, queue, map and upload is bounded; query timeouts, a size cap for the log database, connection caps per client and in total. |
| Malicious or tampered update | A release is installed only if its `SHA256SUMS` carries an Ed25519 signature by a key compiled into the running binary, the binary matches its checksum and reports the expected version. The web UI can only queue a version number; the root helper installs exactly that release from the fixed GitHub repository and never an older one. Starting an update needs a browser session and the password. Details in [Updates](#updates). |

Known residual risks:

- A LAN client can place content from a host it controls under Steam depot
  cache keys, because Steam requests are recognised by their User-Agent.
  Steam verifies chunk checksums, so the effect is failed downloads for other
  clients (denial of service), not code execution.
- NFS with `AUTH_SYS` is unauthenticated and unencrypted. Restrict the export
  to PiCache's host, or use SMB 3.1.1 (optionally sealed).
- Plain DNS between clients and PiCache is unencrypted, as with any LAN
  resolver.
- A LAN host that keeps failing sign-ins for the admin's user name from
  several addresses keeps the user-name delay running for as long as it goes
  on, so a browser that has never signed in (a new device, a private window,
  cleared cookies) can be held up. Browsers that signed in before and
  signed-in sessions are not affected; `picache reset-password` does not end
  such an attack (the delay lives in the running service). A host that
  floods sign-ins from very many addresses can also use up the global
  attempt limit while it continues. The audit log (`auth.login_failed`, per
  client) shows where the attempts come from.

## Updates

How to update is described in
[DEPLOYMENT.md](DEPLOYMENT.md#updates); the binding rules are in
[ARCHITECTURE.md §14](ARCHITECTURE.md#14-releases-and-updates).

### What PiCache trusts

- **Only the release key.** The Ed25519 public keys that may sign releases
  are compiled into the binary (`internal/update/keys.go`). The current key
  is published as [release-key.pem](release-key.pem). Before PiCache trusts
  anything of a release, it downloads `SHA256SUMS` and `SHA256SUMS.sig`
  (at most 64 KiB each) and verifies the signature. Then it downloads the
  binary for this machine (at most 256 MiB), checks its SHA-256 against the
  signed list, runs it once and requires it to report the expected version.
  A file that fails any check is deleted and nothing is replaced.
- **Not GitHub alone.** HTTPS protects the transfer, and the signature proves
  the origin: someone who takes over the GitHub account or changes release
  files, but does not hold the private key, cannot get PiCache to install a
  binary. Release notes are shown in the web UI as text; they are never
  turned into HTML.
- **Not the download location.** The release is looked up by its version in
  the fixed repository `Hustenreizjuengling/PiCache`. No setting, request or
  API response can point PiCache at another URL.
- Only the program is replaced. Unit files and the installer are changed
  only by running `install.sh` yourself.
- The container images on GHCR are not signed. They are built by the same
  workflow from the same tag as the signed binaries. Pin a tag (or a digest)
  in the compose file if you want to decide when the image changes.

### The update helper

The PiCache service runs unprivileged and cannot replace its own program. On
bare metal, VMs and LXC containers the installer adds a root helper
(`picache-update.path` and `picache-update.service`) unless you pass
`--without-updater`:

- The service only writes a request file to `<data>/update-requests/`. It
  contains a version number and who asked for it (for the log), nothing
  else: no URL, file name or command.
- The web UI queues a request only for the version that the last check found
  (always newer than the running one), only when no update is running, and
  only for a browser session that enters the password again (API tokens
  cannot, like restores). The audit log records it as
  `system.update_queued`.
- The helper checks the version string against
  `^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`, refuses an older or the same version,
  downloads that release from the fixed repository and runs the checks above.
  It writes only the installed binary's directory
  (`/usr/local/bin/.picache.update`, `picache.prev`, `picache`), restarts
  `picache.service`, writes its progress to `<data>/update-requests/`, and in
  a rollback puts back `picache.prev` and the pre-upgrade copy of
  `picache.db` from `<data>/backups/`.
- So a compromised service can at most ask for another signed, newer release
  of PiCache (for example a pre-release). It cannot make the helper run
  anything else.
- Without the helper (`--without-updater`, or after deleting
  `/etc/picache/updater.enabled` and disabling `picache-update.path`), updates
  are installed only by an administrator with `sudo picache update`, which
  runs the same checks.

### The update check

- The service asks `https://api.github.com/repos/Hustenreizjuengling/PiCache/releases`
  once a day (and on **Check now**), with `User-Agent: PiCache/<version>`.
  GitHub therefore sees your public IP address and the installed version.
  Nothing else is sent, and no account or token is used.
- The check only reads release information; it never downloads or installs a
  binary. Turn off **Check for updates daily** on **System → Updates** if
  PiCache must not contact GitHub.
- While the repository is private, GitHub answers the check and the
  downloads only for signed-in users. PiCache does not sign in, so it
  reports that the release information is not reachable; install releases by
  hand then (`picache update --from <dir>`), which checks the signature the
  same way.

### The one-line installer

`curl … | sudo sh` trusts the HTTPS connection to GitHub for the script
itself. `get-picache.sh` then trusts nothing else it downloads until the
signature of `SHA256SUMS` verifies against the release key it carries, and
it checks every file it uses against `SHA256SUMS`. It runs everything from
its last line, so a truncated download runs nothing. If you want to check
the script too, download it, compare it with `SHA256SUMS` of the release
(verified as below) and read it before you run it.

### Verifying a release by hand

The workflow that publishes a release checks its signature before it
publishes it. To check a release yourself, for example before the first
installation, download `SHA256SUMS`, `SHA256SUMS.sig`, the files you want and
the public key [docs/release-key.pem](release-key.pem) (from the repository,
not from the release), then run, with OpenSSL 3.0 or later:

```sh
base64 -d SHA256SUMS.sig > SHA256SUMS.sig.bin
openssl pkeyutl -verify -pubin -inkey release-key.pem -rawin \
  -in SHA256SUMS -sigfile SHA256SUMS.sig.bin    # "Signature Verified Successfully"
sha256sum -c --ignore-missing SHA256SUMS        # "OK" for every file you downloaded
```

Both commands must succeed. `SHA256SUMS.sig` is one line of base64: the
Ed25519 signature over the exact bytes of `SHA256SUMS`. LibreSSL (the default
`openssl` on macOS) cannot verify it; use OpenSSL 3 (`brew install openssl@3`).
The key in `release-key.pem` is the same as the base64 key in
`internal/update/keys.go`:

```sh
openssl pkey -pubin -in release-key.pem -outform DER | tail -c 32 | base64
```

Release builds are meant to be reproducible: the build date is the commit
time, and paths, file order, owners and time stamps are fixed. `make dist
VERSION=<tag>` on a clean checkout of the tag with the same Go version
(`picache version` shows it) should give files with the same checksums as
the release.

### The release key

- The private key exists in two places only: the secret
  `RELEASE_SIGNING_KEY` of the GitHub environment `release`, and an offline
  backup kept by the maintainer (for example on an encrypted
  medium). It is never committed; `.gitignore` excludes `*.pem` and `*.key`
  except the public `docs/release-key.pem`.
- Only the `sign` job of the release workflow can read the secret. That job
  checks out no code, runs no third-party actions and gets `SHA256SUMS`
  from the build job as a job output; it writes the key to a private
  temporary file, signs and deletes the file. The build (with `npm ci` and
  the Go build) runs in a job without secrets, so a compromised dependency
  cannot reach the key. The environment `release` only admits `v*` tags.
- GitHub gives no secrets to
  workflows of pull requests from forks, and the release workflow runs only
  for pushed `v*` tags. The privilege to guard is therefore write access to
  the repository; a tag ruleset can also limit who may create `v*` tags.
- **Rotation** (planned, the old key is still safe):
  1. Create the new key offline, as in
     [CONTRIBUTING.md](../CONTRIBUTING.md#one-time-setup-the-signing-key).
  2. Add its raw public key (the command above) to `internal/update/keys.go`,
     keeping the old key, and publish a release. It is still signed with the
     old key, so every installed version accepts it.
  3. Then replace `docs/release-key.pem` with the new public key and the
     secret `RELEASE_SIGNING_KEY` with the new private key. Later releases
     are signed with the new key only.
  4. Versions older than the release of step 2 cannot verify the new
     signatures. Update them to that release first
     (`sudo picache update --version <that release>`, or with the installer),
     and say so in the release notes. Remove the old key from `keys.go` once
     no supported version needs it.
- **Compromised key:** delete the secret at once, create a new key and follow
  the rotation, but remove the old key in step 2 instead of keeping it, and
  publish a security advisory. Every version that still trusts the old key
  accepts anything signed with it, so affected installations must be updated
  by hand, with the files checked against the new `docs/release-key.pem`.

## Notifications

PiCache can send notifications (health problems, cache storage offline,
updates, scheduled backups, sign-in lockouts) to webhooks (for example Home
Assistant), ntfy and Gotify (**System → Notifications**). The binding rules
are in [ARCHITECTURE.md §15.1](ARCHITECTURE.md#151-notifications-internalnotify).

- **Only what the admin configured leaves the host.** Channels are created
  by admins (browser session or admin API token). PiCache sends a `POST` to
  exactly the configured URL (`http` or `https`); it follows no redirects,
  uses no proxy, verifies TLS certificates, gives up after 10 seconds and
  reads at most 4 KiB of the answer, which it discards.
- **Private addresses are allowed on purpose.** The usual receivers run on
  the LAN or on the same host (Home Assistant, a self-hosted ntfy or
  Gotify), so unlike list downloads and the cache proxy, notifications may
  go to RFC 1918, ULA, CGNAT and loopback addresses, and host names are
  resolved by the host's resolver. An admin can therefore make PiCache send
  a `POST` with a notification to a LAN service and see its HTTP status in
  the test result; this needs admin rights, which already control far more
  than that. Link-local addresses (including the cloud metadata address
  169.254.169.254), multicast and unspecified addresses are always refused,
  also after name resolution.
- **Secrets are sealed and write-only.** The webhook `Authorization` value,
  the ntfy access token and the Gotify application token are sealed with the
  master key like NAS passwords, never returned by the API (only whether one
  is stored), never logged or written to the audit log, and dropped from
  backups unless secrets are included. A stored secret is kept only while
  the channel's kind and the URL's scheme, host and port stay the same; to
  point a channel at another server the secret has to be entered again, so
  a stolen admin token cannot redirect a stored secret to its own server.
- **Prefer the secret field to tokens in the URL.** A token in the URL (for
  example ntfy's `?auth=`) is stored in plain text and visible to admins; the
  UI shows URLs without the query string, and the audit log and error
  messages never contain it. Home Assistant webhook ids are part of the URL
  path: treat such URLs as secrets and use `https` where possible.
- **Message content.** Messages may name health checks, storage targets,
  versions, backup files and, for sign-in lockouts, the client address. They
  never contain secrets, passwords, tokens, session data or user names (a
  user name can be a mistyped password).
- **Bounded.** At most 10 channels, 32 queued messages and 20 messages in
  10 minutes per channel (then one summary), 3 attempts per message; the
  delivery log keeps the last 200 attempts in memory only.

## Scheduled backups

Scheduled backups (**System → Backup & restore**) have exactly the content
of a backup downloaded in the UI: never accounts, password hashes,
sessions or API tokens, and sealed NAS passwords and notification secrets
only when "include sealed secrets" is on (they are useless without the
master key, which is never part of a backup). The binding rules are in
[ARCHITECTURE.md §15.2](ARCHITECTURE.md#152-scheduled-backups).

- They are written to `<data>/backups/scheduled/` or to
  `picache-backups/` in the store of an online storage target, as files
  of mode 0640 in a directory of mode 0750 created by PiCache. Files are
  written under a temporary name (created exclusively, never through a
  symbolic link) and renamed when complete; a symbolic link in place of the
  directory is refused.
- Retention deletes only files named exactly like this installation's
  scheduled backups (`picache-backup-<instance id>-<time>.db`); nothing else
  in the directory is touched.
- Anyone who can read the destination (for example other users of the NAS
  share) can read the configuration, the audit log and the client list in
  these files. Restrict the share like other sensitive data.
- Changing the schedule, running a backup, downloading and deleting
  backups need admin rights and are audited; read-only users see the status
  and the file names.

## Parental controls and the network check

The binding rules are in
[ARCHITECTURE.md §16](ARCHITECTURE.md#16-parental-controls-internaldnsparental)
and [§17](ARCHITECTURE.md#17-network-check-internalappnetcheckgo).

- **Parental controls are DNS-based and can be bypassed.** They block the
  names a device looks up through PiCache. A device that uses another
  resolver (a DNS server set by hand, encrypted DNS in an app or browser, a
  VPN app, mobile data or another network) is not restricted, and answers
  the device cached before a block, as well as connections that are already
  open, keep working until they expire or reconnect. The catalogue list
  "HaGeZi DoH/VPN/TOR/Proxy Bypass" and blocking outgoing DNS (ports 53 and
  853) to other servers on the router make bypassing harder; neither is
  complete. Treat parental controls as a help for a household, not as a
  security boundary against a determined user of the device.
- **Changes need admin rights and are audited** (`parental.update`,
  `parental.override`, `parental.override_clear`); read-only users and read
  tokens see the configuration and the state. Pausing the blocklists does
  not lift parental controls; only an admin action does. Service domains
  come from a catalogue compiled into the binary; nothing is downloaded.
- **The network check only reads.** It uses the kernel's neighbour table,
  the routing tables, the query statistics PiCache keeps anyway and a PTR
  lookup of the gateway through the router resolver; nothing leaves the
  host. The addresses refused by the DNS ACL are counted in memory only (at
  most 256), never logged per packet or stored.
- **The discovery scan** (admins only, audited as `network.scan`) sends one
  empty UDP datagram to the discard port (9) of each address of this
  machine's private IPv4 subnets: /24 or smaller subnets in full, larger
  ones only the /24 around this machine's address, at most 512 addresses,
  at most 200 packets per second, at most one scan per minute. It uses an
  ordinary unprivileged UDP socket: no raw sockets, no capabilities,
  nothing is read back; devices show up because the kernel resolves their
  addresses with ARP. It is not available in a container bridge network, on
  systems other than Linux, or for IPv6. A scan can wake devices that sleep
  and may appear in the logs of intrusion detection on the LAN.

## Hardening checklist

**Network**

- [ ] PiCache is not reachable from the Internet (no port forwarding; a host
      firewall if the machine has a public interface).
- [ ] `allowAllNetworks` is off. Additional client networks are added as
      narrow CIDRs only.
- [ ] The machine has a static address, and routers do not hand out another
      resolver (DHCP or IPv6 RA) that bypasses the filter.

**Web UI**

- [ ] Setup was completed right after the first start (the setup token grants
      the admin account to whoever uses it first).
- [ ] The admin password is long and unique, and TOTP is enabled
      (**System → Account & security**).
- [ ] The UI is used over HTTPS (`:8443`, ideally with your own certificate
      via `PICACHE_WEB_TLS_CERT`/`PICACHE_WEB_TLS_KEY`), and the HTTPS redirect
      is enabled in the web settings. Optionally disable plain HTTP with
      `PICACHE_WEB_LISTEN=off`. The sign-in and setup pages point to the
      HTTPS port when they are opened over plain HTTP.
- [ ] Behind a TLS-terminating reverse proxy that talks plain HTTP to
      PiCache, `PICACHE_WEB_SECURE_COOKIES=true` is set, so the session
      cookie is `Secure`.
- [ ] API tokens use scope `read` wherever possible and have an expiry.
      Unused tokens and sessions are revoked.
- [ ] After a suspected compromise the password is changed in the UI
      (this ends all other sessions and all API tokens) or reset with
      `picache reset-password <user>` (also disables TOTP), and the audit log
      is checked.
- [ ] `/metrics` stays disabled unless you scrape it (it then requires an
      admin token).
- [ ] Only needed host names are in `PICACHE_WEB_HOSTS` / the allowed hosts.

**Deployment**

- [ ] Bare metal / LXC: the shipped `picache.service` is used unchanged; local
      changes are drop-ins that do not remove sandboxing options.
- [ ] Docker: the shipped compose settings are kept (`cap_drop: [ALL]`,
      `no-new-privileges`, `read_only`); never `privileged`, never
      `SYS_ADMIN`.
- [ ] No `PICACHE_ADMIN_PASSWORD` in the environment; if
      `PICACHE_ADMIN_PASSWORD_FILE` was used, the file and the variable were
      removed after the first start.
- [ ] `/etc/picache/picache.env` is `0640 root:picache`,
      `/etc/picache/credentials` is `0700 root`.
- [ ] The system clock is synchronised (NTP). Encrypted upstreams depend on
      it. While the clock is before the binary's build date, PiCache falls
      back to plain DNS to its bootstrap servers. A build without a build
      date (such as the image `docker compose up --build` builds) has no such
      fallback: its encrypted upstreams fail until the clock is right.
- [ ] Host-apply (`--with-host-apply`) is installed only if you use it.
      To disable the root helper, delete `/etc/picache/host-apply.enabled`
      and run `systemctl disable --now picache-storage.path`.
- [ ] NAS targets that are no longer used are deleted in the UI (the root
      helper then removes their mount unit and credentials file), or with
      `picache storage remove <id>` when the helper is not installed.

**Data and secrets**

- [ ] Backups of `picache.db` are stored like other sensitive data. Backups
      downloaded in the UI or API and scheduled backups hold the
      configuration and the audit log (never accounts, password hashes,
      sessions or API tokens); file copies of `picache.db` hold everything.
      Backups "including secrets" are made only when needed.
- [ ] A NAS share that receives scheduled backups is readable only by
      PiCache and the people who may see the configuration.
- [ ] Notification channels use `https` where the receiver supports it,
      and access tokens go into the secret field, not into the URL.
- [ ] `keys/master.key` is backed up separately from the database (or the key
      comes from a systemd credential / Docker secret). A systemd credential
      is given to `picache-storage.service` too when host-apply mounts SMB
      shares with a stored password.
- [ ] The NAS account can access only the cache share. NFS exports are limited
      to PiCache's host.
- [ ] The query-log retention and client anonymisation settings match your
      privacy requirements.
- [ ] `downloadCache.nocacheClients` and `allowPrivateUpstreams` stay at their
      defaults (empty / off) unless you know you need them.

**Maintenance**

- [ ] PiCache is kept up to date (**System → Updates**, or
      `picache update --check` in your monitoring). The CI runs
      `govulncheck` against the dependencies.
- [ ] The update helper is installed only if you want to update from the
      web UI (otherwise `--without-updater`). The daily update check is off
      if PiCache must not contact GitHub.
- [ ] Before the first installation, the release files were checked with
      `sha256sum -c` and their signature
      ([Verifying a release by hand](#verifying-a-release-by-hand)).
- [ ] The health page (**System → Health & about**) and the audit log are
      reviewed now and then.
