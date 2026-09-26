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
| Open resolver, DNS amplification | Answers only loopback, private (RFC 1918, ULA, CGNAT, link-local) and directly connected private networks, plus CIDRs you add. "Allow every network this machine is connected to" (`dns.trustConnectedNetworks`, off by default) also answers the public networks of its interfaces (virtual bridges and tunnels excluded), for LANs with a global IPv6 prefix that changes; on a cloud server or VPS the connected network can contain other customers, so leave it off there. Other UDP queries are dropped; TCP connections are closed at accept. Per-client rate limit (default 50 qps, burst 200), `ANY` refused, CHAOS/`version.bind` refused, EDNS capped at 1232 bytes. `allowAllNetworks` is an explicit, dangerous switch. |
| DNS cache poisoning | Encrypted upstreams by default (DNS-over-HTTPS; DNS-over-TLS is also supported), random IDs and ports, the question is verified on every reply, in-flight deduplication, no client EDNS options forwarded. |
| Private reverse-DNS leaks | PTR/SOA/NS queries for private and special-use reverse zones (and the extra networks of `dns.privateReverseNetworks`) never reach public upstreams, fallbacks or `default` forwarders. Address queries (A, AAAA, HTTPS, SVCB, ANY) for bare names (`nas`) are answered from the local domain and never sent to the upstreams (`dns.domainNeeded`, on by default); other types of bare names (e.g. `NS` or `DS` of top-level domains) still are, so validating resolvers behind PiCache keep working. |
| DNS rebinding through PiCache's answers | Answers of the upstreams that point names at private, loopback or link-local addresses are blocked (`dns.rebindProtection`, on by default), also while blocking is paused. Details in [DNS protection](#dns-protection). |
| Clients claiming another identity | Only forwarders listed in `dns.ednsClientTrusted` may name their clients by EDNS; their clients must not be able to send their own options. Details in [DNS protection](#dns-protection). |
| Unwanted clients in the LAN | `dns.blockedClients` drops the DNS queries of addresses, networks or MAC addresses; loopback, this machine, the router and trusted forwarders can never be blocked, neither by their addresses nor by their MAC addresses. DNS only. |
| Open HTTP proxy / SSRF via :80 | Only hosts of known cache services are served; unknown hosts get 403. Upstream addresses must be public unicast and not this machine; link-local (cloud metadata) is always refused, and redirects are re-checked. Non-canonical paths are never stored. Per-client fill limits and at most 16 ranges per request prevent WAN amplification. |
| Open TLS relay via :443 | TLS is never terminated. The SNI must match an enabled service, the same SSRF rules apply, and connections are capped and time out. |
| Hostile blocklists, cache-domains data or NAS content | Sizes, counts, names and patterns are validated; public-suffix patterns are rejected. Files are accessed through `os.Root`. Slice files carry validated, CRC-checked headers. |
| Web UI takeover on first start | One-time setup token (logged and stored with mode 0600, compared in constant time, first user created atomically), or provisioning from `PICACHE_ADMIN_PASSWORD_FILE`. After setup, `/auth/setup` answers 403 at once, uses no share of the global attempt limit and counts as a failed attempt of the caller. |
| Password guessing | argon2id; per client 5 failures → 15 min lockout; per user name a delay from the 5th failure (1 s, doubling, at most 30 s, reset by a successful sign-in), never a lockout; TOTP failures count; a global attempt limit; optional TOTP with single-use codes. Every sign-in gives the browser a device cookie (sealed with the master key, `HttpOnly`, 180 days, not a credential): a browser that signed in before is throttled by its own device key (5 failures → 15 min) instead of the user-name delay, so failed attempts from other LAN hosts cannot keep it out, and a copied device cookie allows no more guesses than one client. Password confirmations of signed-in users (tokens, TOTP, password change, restore) are throttled per client and per session (5 failures → 15 min each) and by the global limit, not by the user-name delay. |
| Session theft, CSRF | 256-bit session tokens stored hashed; cookie `HttpOnly`, `SameSite=Strict`; over HTTPS `Secure` and named `__Host-picache_session`, so a plain-HTTP origin on the same host cannot plant or overwrite it; behind a TLS-terminating reverse proxy the same applies when the proxy is trusted and sends `X-Forwarded-Proto: https`, else `PICACHE_WEB_SECURE_COOKIES` sets `Secure`; idle and absolute timeouts; sessions re-checked on every request; cross-origin protection; JSON-only request bodies. A stolen session alone cannot create API tokens or enrol TOTP (both need the current password); changing the password ends all other sessions and all API tokens (unless kept explicitly); enabling TOTP ends all other sessions. |
| API token misuse | Tokens have scope `read` or `admin` and can never manage tokens, passwords, TOTP or sessions, nor restore a backup, install an update, manage accounts or change the HTTPS certificate (these need an interactive session; accounts, restores, updates and certificates an admin's). An admin token acts with admin rights only while its owner is an admin (checked on every request); demoting an account deletes its admin tokens. Backups never contain accounts (users, password hashes, TOTP secrets, sessions, tokens), and a restore keeps the accounts and tokens of the running instance, so an admin token cannot become the interactive account. Live streams re-check the token every 15 s. |
| Malicious backup upload | A restore needs an interactive session and the current password. Uploads with triggers, views, virtual tables, generated columns, tables or indexes the running PiCache does not have, indexes defined differently from the running PiCache's (including named indexes disguised as automatic ones), or altered account tables are refused, at upload and again at the next start. The restore keeps the accounts, API tokens and audit log of the running instance and ends all sessions. Every database connection runs with `trusted_schema` off. PiCache creates no triggers or views: any found in `picache.db` (e.g. planted through a restore by an older version) are removed at start with a warning, by `picache reset-password`, and from every backup copy; revoking sessions or tokens and scrubbing a backup verify that the rows are really gone. |
| DNS rebinding against the UI | Host allowlist (IP addresses, localhost, this machine's names, configured hosts); other hosts get 421. |
| Web UI reachable from other networks | New installations allow the web UI only from this machine, the private ranges, the networks the machine is connected to, the DNS allowed networks and the addresses you add (**System → Users & security → Web access**); other connections are closed at accept, and every request is checked again. A change that would lock out your own address is refused (this machine always passes), and `picache web-access --reset` on the host opens the web UI again. Installations upgraded from 0.10 keep it open until you switch the restriction on. |
| Forged client addresses through proxies | `X-Forwarded-For` and `X-Forwarded-Proto` are read only from the addresses in `web.trustedProxies` (at least /24 or /64, never everything), right-most entry first; `Forwarded`, `X-Real-IP` and similar headers are always ignored; a malformed entry before the client fails the request. Residual: a trusted proxy that passes a client's `X-Forwarded-For` through without appending lets that client choose its address; trusting loopback trusts every local process; a proxy on the same host that is *not* trusted makes every client appear as loopback (always allowed, one shared sign-in throttle); never list a whole LAN. |
| Too much power per account | Accounts are admins or viewers; viewers see the pages read-only (not the audit log, notification channels, backup downloads or storage snippets) and change only their own password, two-factor authentication, sessions and read tokens. Accounts are managed only in an admin's browser session with the current password; at least one admin always remains, also when two admins demote each other at the same time. A role change ends the account's sessions at once; a token or sign-in requested while a demotion or password reset is saved is refused, never created after it. |
| Configuration drift in managed installations | `PICACHE_CONFIG_LOCKED` refuses configuration changes from interactive sessions (pauses, overrides, refreshes and tests stay possible); the automation writes with an admin API token. It is **not** an access control against admins: admin tokens still write, and whoever controls the host can unset it. `PICACHE_DESTRUCTIVE_API=false` refuses restores, resets, purges and other bulk deletions for everyone. |
| Web certificate and the local CA | The local CA is limited by critical name constraints to PiCache's own names and addresses (never the local domain, other LAN names, `web.allowedHosts` or other addresses), with path length 0. Its key stays in the data directory (0600) so renewals need no new trust on your devices. Residual risk: whoever can read the data directory can issue certificates that devices which imported the CA trust, but only for PiCache's own names and addresses, whose server key the same attacker already holds. Remove the CA from your devices when PiCache is retired or its data directory was exposed, and create a new CA after a compromise. Uploaded private keys are accepted only over HTTPS or from a loopback address on the host (such as `http://127.0.0.1:8080`; the host's LAN address over plain HTTP does not count), with the current password, and are never shown, logged or audited. |
| XSS, clickjacking | Strict Content-Security-Policy, `X-Frame-Options: DENY`, `nosniff`, `no-referrer`. The UI never renders HTML from data. |
| Secret leakage | NAS passwords, notification secrets and TOTP secrets are sealed (XChaCha20-Poly1305) with a master key that is never part of a backup. Secrets are write-only in the API and redacted in logs, snippets, notifications and the audit log. Only the root helper decrypts NAS passwords. CDN query strings are never logged or stored. |
| Outbound notifications | Only admins configure channels. PiCache sends only to the URLs they entered (http or https, no redirects, no proxy, verified TLS, 10 s), and never to link-local (cloud metadata), multicast or unspecified addresses. Private and loopback addresses are allowed on purpose. A stored secret is never sent to a changed server. Messages carry no secrets, passwords, tokens, session data or user names. Details in [Notifications](#notifications). |
| Scheduled backups | Same content as downloaded backups (no accounts; sealed secrets only on request), written only to the data directory or to a storage target's store, without following symbolic links. Details in [Scheduled backups](#scheduled-backups). |
| Network discovery scan | Admins only, audited, at most one per minute: one empty UDP datagram per address of this machine's private IPv4 subnets (at most 512, at most 200 per second) from an unprivileged socket; no raw sockets or capabilities. Details in [Parental controls and the network check](#parental-controls-and-the-network-check). |
| DHCP server (optional) | Off by default; switched on in the web UI by an admin (`PICACHE_DHCP=off` prevents it); no DHCP port is open while it is off. Serves one chosen interface, never relayed requests, never while its own address is dynamic or another DHCP server was detected. Every DHCPv4, DHCPv6 and ICMPv6 packet is parsed with strict bounds checks, rate limited and dropped when malformed; replies cannot be aimed at hosts outside the LAN. Router advertisements never make PiCache a router. `CAP_NET_RAW` is used only at start (while router advertisements are on) and then dropped on every thread; PiCache refuses to run if that fails. Details in [DHCP server](#dhcp-server). |
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
- DHCP and router advertisements are unauthenticated protocols: with the
  optional DHCP server, a device on the LAN can exhaust the address pool or
  run a competing DHCP server. PiCache's limits contain the effect but
  cannot prevent it (see [DHCP server](#dhcp-server)).
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
- The program and the systemd unit files are replaced, nothing else. The
  unit files come only from the signed release archive
  (`picache-deploy.tar.gz`, checked against the signed `SHA256SUMS` and
  unpacked in memory with size limits), only PiCache's own unit names, only
  over unit files that already exist as regular files in
  `/usr/local/lib/systemd/system` (the old one is kept as `<unit>.prev`),
  never drop-ins. If replacing them fails, they are put back and only the
  program is updated. The installer itself changes only when you run it.
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
  (`/usr/local/bin/.picache.update`, `picache.prev`, `picache`) and the unit
  directory (`/usr/local/lib/systemd/system`: the release's unit files and
  their `.prev` copies, then `systemctl daemon-reload`), restarts
  `picache.service`, writes its progress to `<data>/update-requests/`, and in
  a rollback puts back the unit files it replaced, `picache.prev` and the
  pre-upgrade copy of `picache.db` from `<data>/backups/`. The service still
  passes it nothing but a version string. A helper unit from before 0.8.0
  cannot write the unit directory; run the one-line installer once to get
  the current units.
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

## DHCP server

The binding rules are in
[ARCHITECTURE.md §18](ARCHITECTURE.md#18-dhcp-server-internaldhcp). The DHCP
server is optional and off by default; these are its trade-offs.

- **Nothing open while off.** UDP ports 67 and 547 are open only while the
  DHCP server is switched on (a search for other DHCP servers opens port 67
  for its 5 seconds); port 546 (the host's own DHCPv6 client) is never
  bound. `PICACHE_DHCP=off` (`install.sh --without-dhcp`) prevents switching
  it on. Two empty marker files in the data directory (`dhcp.sockets`,
  `dhcp.ra`) tell the next start which sockets to open; they grant nothing
  the settings do not (the service can change the settings anyway), and
  PiCache only checks that they exist (`lstat`), never reads them. The
  installer creates them once in place of a removed `PICACHE_DHCP=on` (an
  empty file owned by `picache`, never through a symbolic link: an existing
  link or file is left alone).
- **Capabilities.** Ports 67 and 547 need `CAP_NET_BIND_SERVICE`, which
  the service has anyway (so it can open them while running). IPv6 router
  advertisements need a raw ICMPv6 socket and therefore `CAP_NET_RAW`,
  which every systemd unit grants (Docker: `cap_add: [NET_RAW]`). PiCache
  uses it only at start, and only while router advertisements are on, to
  open that single socket before it touches any file. On every start it
  then drops `CAP_NET_RAW` from the effective, permitted, inheritable and
  ambient sets of every thread (the unit re-allows the `capset` system call
  for that), checks the capability sets of all threads and that a new raw
  socket is refused. **Fail closed:** if the capability cannot be dropped,
  PiCache refuses to run; if the thread capabilities cannot even be read
  (so the drop cannot be verified), it closes the raw socket, sends no
  advertisements and fails the health check `dhcp`, but keeps DNS running.
  The raw socket accepts only router solicitations and advertisements (an
  ICMPv6 filter), and the kernel fills in the checksums.
- **Residual risk of router advertisements.** While they are on, the raw
  socket stays open. A compromised PiCache process could use it to send any
  ICMPv6 message from the machine's link-local address: spoofed router
  advertisements, neighbour advertisements or redirects, i.e. a LAN-wide
  IPv6 man in the middle. Switch router advertisements on only when your
  router cannot announce PiCache itself; they are off by default.
- **Untrusted packets.** Any device on the LAN can send DHCP and ICMPv6
  packets. The parsers are written for this: size limits (DHCPv4 240 to
  1500 bytes, DHCPv6 4 to 1500 bytes), every option length checked against
  the packet, no option overload, no recursion, memory bounded by the
  packet size; malformed packets are dropped and counted.
  A failure while handling one packet drops only that packet, and DNS keeps
  running whatever the DHCP part does. Fuzz tests cover the parsers.
- **Abuse from the LAN.** A device on the LAN can exhaust any DHCP pool
  (many MAC addresses) or answer DHCP itself; that is inherent to DHCP.
  PiCache limits the effect: at most 50 DHCPv4 packets per second (5 per
  MAC), 50 DHCPv6 packets per second, one answer to router solicitations
  per 3 s, at most 50 other routers' advertisements parsed per second,
  bounded tables (4096 leases, 1024 reservations, 32 other servers and 32
  other IPv6 announcers, 200 logged exchanges), a DECLINE is accepted only
  for the address the device holds or was offered. Reservations by MAC
  address or client identifier (option 61) and "only reserved devices" are
  conveniences, not access control: both identifiers can be forged. Replies go
  to the limited broadcast address, to an address inside the served subnet,
  or (DHCPv6) to a link-local address, so they cannot be reflected at hosts
  elsewhere. Host names from devices are reduced to plain DNS labels, and
  `wpad`, `localhost` and names of the form of a generated name
  (`192-168-1-5`) count as none, so a device cannot take the WPAD name or
  another address's name; local DNS records and PiCache's own names always
  win over them. A WPAD URL option is sent only to clients that ask for it,
  but then every such client uses that proxy configuration: changing it is
  audited with the new value.
- **Two DHCP servers.** PiCache looks for other DHCP servers before it
  serves (and every 10 minutes while it serves) and refuses to start while
  one answers or a device asked one within 24 hours. The search is a relayed
  DISCOVER; a server that ignores relayed requests is only noticed when a
  device chooses its offer. `ignoreOtherServers` overrides the check and is
  meant only for a second server that serves other devices.
- **IPv6.** PiCache's router advertisements carry only the DNS server and
  the domain with router lifetime 0: they cannot make PiCache a router or
  change addresses and routes. A rogue device can still send its own
  router advertisements; use RA guard on managed switches where that
  matters. PiCache's search for other IPv6 announcers sends only a router
  solicitation and a relayed DHCPv6 information request (never a Solicit
  or an address request, so it creates no binding on a router), and parses
  the answers with the same bounds checks and rate limits.

## Parental controls and the network check

The binding rules are in
[ARCHITECTURE.md §16](ARCHITECTURE.md#16-parental-controls-internaldnsparental)
and [§17](ARCHITECTURE.md#17-network-check-internalappnetcheckgo).

- **Parental controls are DNS-based and can be bypassed.** They block the
  names a device looks up through PiCache. A device that uses another
  resolver (a DNS server set by hand, encrypted DNS in an app or browser, a
  VPN app, mobile data or another network) is not restricted, and answers
  the device cached before a block, as well as connections that are already
  open, keep working until they expire or reconnect. The category switch
  for encrypted DNS and VPN bypass (the list "HaGeZi DoH/VPN/TOR/Proxy
  Bypass") and blocking outgoing DNS (ports 53 and 853) to other servers on
  the router make bypassing harder; neither is complete. Treat parental
  controls as a help for a household, not as a security boundary against
  a determined user of the device.
- **Safe search is DNS-based too.** PiCache answers the search engines'
  names with a CNAME to their restricted hosts. An app or browser with its
  own DNS, a VPN, another network or an engine PiCache does not know gets
  around it; a page the engine does not classify as adult stays visible.
  The unrestricted answer is never returned: if the restricted host cannot
  be resolved, the query fails (SERVFAIL).
- **Category switches download third-party lists.** Service domains come
  from a catalogue compiled into the binary, but the category switches
  (adult content, gambling, dating, piracy, bypass) and every catalogue
  list are downloaded from their maintainers like any other list. The
  catalogue's URLs were checked for the release (reachable, parseable,
  almost no invalid lines); the list content is still the maintainer's,
  can change at any time and can block too much. The parser refuses to
  let a list block a whole top-level domain or ICANN public suffix, as a
  domain entry (`||com^`, `*.co.uk`) or as a pattern (`||*.com^`,
  `.com^`, `/\.xyz$/`, anything that matches a made-up name directly
  below such a suffix), except in lists of the category `abused-tlds`;
  the refused entries are counted per list. A pattern can still block a
  large part of a TLD (`||a*.com^`). A downloaded list is only ever used
  for DNS answers.
- **A category switch is on only while its list is a protection list.**
  A switch counts its list only while the list is a blocklist of the
  switch's category; a list that was given another category (for example
  `security`, which pauses with blocking) or made an allowlist leaves the
  switch off, and switching on gives the list its category back.
- **What a pause keeps in force.** Pausing blocking (globally, or the
  filtering of one group) stops the lists and rules of that scope,
  **the security lists (malware, phishing) included**. Parental controls,
  safe search and the protection lists (the categories adult, gambling,
  dating, piracy and DNS/VPN bypass) stay in force, and so do the
  special domains (the Firefox DoH canary and iCloud Private Relay), so a
  browser does not switch to encrypted DNS during a pause and keep it.
- **Content protection is not lifted by "lift restrictions".** The allow
  override of a group lifts only its blocked services and schedules. Safe
  search and protection lists end only when an admin switches them off,
  disables the group or moves the device out of it; a user allow rule for
  the device unblocks a single name a protection list blocks by mistake.
- **Changes need admin rights and are audited** (`parental.update` — with
  the services, schedules, safe search and category switches —,
  `parental.override`, `parental.override_clear`, `parental.pause`,
  `parental.pause_clear`, and `filter.list.create`/`filter.list.update`
  for the lists a category switch creates or changes, written as soon as
  the lists changed). If the parental configuration cannot be saved
  afterwards, the list changes are undone exactly (a list the request
  created is deleted, a changed list gets its previous state back) and
  the undo is audited as well (`filter.list.delete`/`filter.list.update`);
  read-only users and read tokens see the configuration and the state. Pausing the blocklists
  does not lift parental controls; only an admin action does.
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

## DNS protection

**DNS rebinding.** A web page can point one of its own names at an address
in your LAN (a router's web interface, a NAS, a service on the device
itself) and then talk to it through the browser. Many routers block such
answers, but once the devices use PiCache the router no longer sees the
queries. PiCache therefore blocks answers of its upstreams (the default
upstreams, the fallbacks and conditional forwarders with the target
`default`) whose A/AAAA records point at 0.0.0.0/8, 10/8, 100.64/10,
127/8 (it protects services on the client itself), 169.254/16, 172.16/12,
192.168/16, `::`, `::1`, fc00::/7, fe80::/10 or 64:ff9b:1::/48, or at an
IPv6 address that carries such an IPv4 address (IPv4-mapped,
IPv4-compatible, 6to4, NAT64 and the configured DNS64 prefix). It removes
such addresses from the hints of HTTPS/SVCB records and from the additional
section. The protection is on by default and applies while blocking is
paused, like the ACL. Local data (local records, DHCP names, forwarders
with their own targets, the router, the local domain) is trusted as
configured; `dns.rebindAllow` (by default `plex.direct`), the names of
`web.allowedHosts` and user allow rules exempt names on purpose.

**Client identity from EDNS.** A forwarder (a second router, dnsmasq) can
add each client's address (ECS) and MAC (option 65001) to the queries it
forwards. PiCache believes them only from the addresses in
`dns.ednsClientTrusted`, only in a strict form (exactly one option of each
kind, a full-length unicast address, a unicast MAC), and never lets them
into the neighbour table or the device data. The identity decides groups,
parental controls, rules and the query log, so **a trusted forwarder must
strip or replace the options its own clients send** (dnsmasq:
`--strip-subnet --strip-mac --add-subnet=32,128 --add-mac`); otherwise any
client behind it can claim another device's identity and escape its
parental controls or rules. The transport address stays in charge of the
ACL, the blocked clients, the rate limit, the loop guard and this server's
names, so a trusted source cannot turn a derived address into more rights
on the DNS server itself.

**Client subnet (ECS) privacy.** Off by default. Mode `client` sends the
/24 (IPv4) or /56 (IPv6) of a public client address to the default
upstreams, and discloses the household's IPv6 prefix also for queries that
go out over IPv4; mode `custom` sends a fixed public network. The subnet
is never sent to conditional forwarders with their own targets, the
router, the bootstrap servers or with PiCache's own lookups, and a reply
that carries another subnet is discarded. Client subnets sent by clients
are never forwarded (they are only logged, anonymised with the client
addresses).

**Fastest-address probes.** With the upstream mode `fastest_addr` (off by
default) PiCache opens TCP connections to the addresses of answers, which
the owner of a queried name chooses. The probes are bounded: only public
unicast addresses that are not this machine (the rules of the download
cache's SSRF guard), at most 8 per answer, a connect to 443 (80 only after
443 failed) and an immediate close without data or TLS, 300 ms per answer,
at most 32 at a time and 100 new addresses per second (beyond that nothing
is probed), results cached for 10 minutes. They tell the remote side that
someone behind PiCache resolved the name.

**Answers blocked by the upstream.** Quad9 and Cloudflare's security
resolver block malware domains themselves. PiCache recognises their blocks
(EDE 15–17, 0.0.0.0/`::`, block pages, Quad9's NXDOMAIN without the RA
flag), shows them as blocked and names only the upstream's host (never the
path of a DoH URL, which can carry a profile ID); the upstream's own error
text is never passed on to clients.

## Hardening checklist

**Network**

- [ ] PiCache is not reachable from the Internet (no port forwarding; a host
      firewall if the machine has a public interface).
- [ ] `allowAllNetworks` is off. Additional client networks are added as
      narrow CIDRs only. `trustConnectedNetworks` is off on machines whose
      network is shared with others (cloud servers, VPS).
- [ ] The machine has a static address, and routers do not hand out another
      resolver (DHCP or IPv6 RA) that bypasses the filter.
- [ ] DNS rebinding protection stays on (`dns.rebindProtection`), and only
      domains that really answer with LAN addresses are allowed. Trusted EDNS
      forwarders (`dns.ednsClientTrusted`) strip the ECS and MAC options of
      their own clients. The client subnet (`dns.ecs`) stays off unless a
      CDN needs it.

**Web UI**

- [ ] Setup was completed right after the first start (the setup token grants
      the admin account to whoever uses it first).
- [ ] The admin password is long and unique, and TOTP is enabled
      (**System → Users & security**).
- [ ] The web UI is restricted to your networks (**System → Users & security
      → Web access**, "Allow the web UI only from these networks"); an
      installation upgraded from 0.10 has it off until you switch it on.
- [ ] Only the exact addresses of your reverse proxies are trusted
      (`web.trustedProxies`), never a whole LAN or loopback unless the proxy
      runs on the same host; a proxy on the same host is always listed.
- [ ] People who only look get viewer accounts, not the admin password; every
      person has an own account (the audit log names who changed what).
- [ ] Managed installations set `PICACHE_CONFIG_LOCKED=on` (changes then come
      from an admin API token only) and, where restores and purges must not
      happen through the API, `PICACHE_DESTRUCTIVE_API=false`.
- [ ] The UI is used over HTTPS (`:8443`): your devices trust PiCache's local
      CA (**System → HTTPS certificate**), or you uploaded your own certificate
      or use certificate files (`PICACHE_WEB_TLS_CERT`/`PICACHE_WEB_TLS_KEY`,
      reloaded when renewed), and the HTTPS redirect is enabled in the web
      settings. Optionally require TLS 1.3 (`web.tlsMinVersion`). Optionally disable plain HTTP with
      `PICACHE_WEB_LISTEN=off`. The sign-in and setup pages point to the
      HTTPS port when they are opened over plain HTTP.
- [ ] Behind a TLS-terminating reverse proxy that talks plain HTTP to
      PiCache, the proxy is in `web.trustedProxies` (then its
      `X-Forwarded-Proto: https` makes the session cookie `Secure`), or
      `PICACHE_WEB_SECURE_COOKIES=true` is set.
- [ ] API tokens use scope `read` wherever possible and have an expiry.
      Unused tokens and sessions are revoked.
- [ ] After a suspected compromise the password is changed in the UI
      (this ends all other sessions and all API tokens) or reset with
      `picache reset-password <user>` (also disables TOTP), and the audit log
      is checked. If the local CA's key may have been read, a new local CA is
      created and the old one removed from your devices.
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
- [ ] The DHCP server is switched on only if PiCache hands out addresses;
      then the machine has a static address and the router's DHCP server is
      off. On a host that runs another DHCP server, set `PICACHE_DHCP=off`
      (`install.sh --without-dhcp`). IPv6 router advertisements are on only
      if the router cannot announce PiCache itself.
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
