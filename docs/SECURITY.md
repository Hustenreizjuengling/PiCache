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

PiCache has no releases yet. Security fixes go to the `main` branch; please
test against a current build of `main`.

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
| Open HTTP proxy / SSRF via :80 | Only hosts of known LanCache services are served; unknown hosts get 403. Upstream addresses must be public unicast and not this machine; link-local (cloud metadata) is always refused, and redirects are re-checked. Non-canonical paths are never stored. Per-client fill limits and at most 16 ranges per request prevent WAN amplification. |
| Open TLS relay via :443 | TLS is never terminated. The SNI must match an enabled service, the same SSRF rules apply, and connections are capped and time out. |
| Hostile blocklists, cache-domains data or NAS content | Sizes, counts, names and patterns are validated; public-suffix patterns are rejected. Files are accessed through `os.Root`. Slice files carry validated, CRC-checked headers. |
| Web UI takeover on first start | One-time setup token (logged and stored with mode 0600, compared in constant time, first user created atomically), or provisioning from `PICACHE_ADMIN_PASSWORD_FILE`. After setup, `/auth/setup` answers 403 at once, uses no share of the global attempt limit and counts as a failed attempt of the caller. |
| Password guessing | argon2id; per client 5 failures → 15 min lockout; per user name a delay from the 5th failure (1 s, doubling, at most 30 s, reset by a successful sign-in), never a lockout; TOTP failures count; a global attempt limit; optional TOTP with single-use codes. Every sign-in gives the browser a device cookie (sealed with the master key, `HttpOnly`, 180 days, not a credential): a browser that signed in before is throttled by its own device key (5 failures → 15 min) instead of the user-name delay, so failed attempts from other LAN hosts cannot keep it out, and a copied device cookie allows no more guesses than one client. Password confirmations of signed-in users (tokens, TOTP, password change, restore) are throttled per client and per session (5 failures → 15 min each) and by the global limit, not by the user-name delay. |
| Session theft, CSRF | 256-bit session tokens stored hashed; cookie `HttpOnly`, `SameSite=Strict`; over HTTPS `Secure` and named `__Host-picache_session`, so a plain-HTTP origin on the same host cannot plant or overwrite it; `PICACHE_WEB_SECURE_COOKIES` sets `Secure` behind a TLS-terminating reverse proxy; idle and absolute timeouts; sessions re-checked on every request; cross-origin protection; JSON-only request bodies. A stolen session alone cannot create API tokens or enrol TOTP (both need the current password); changing the password ends all other sessions and all API tokens (unless kept explicitly); enabling TOTP ends all other sessions. |
| API token misuse | Tokens have scope `read` or `admin` and can never manage tokens, passwords, TOTP or sessions, nor restore a backup. Backups never contain accounts (users, password hashes, TOTP secrets, sessions, tokens), and a restore keeps the accounts and tokens of the running instance, so an admin token cannot become the interactive account. Live streams re-check the token every 15 s. |
| Malicious backup upload | A restore needs an interactive session and the current password. Uploads with triggers, views, virtual tables, generated columns, tables or indexes the running PiCache does not have, indexes defined differently from the running PiCache's (including named indexes disguised as automatic ones), or altered account tables are refused, at upload and again at the next start. The restore keeps the accounts, API tokens and audit log of the running instance and ends all sessions. Every database connection runs with `trusted_schema` off. PiCache creates no triggers or views: any found in `picache.db` (e.g. planted through a restore by an older version) are removed at start with a warning, by `picache reset-password`, and from every backup copy; revoking sessions or tokens and scrubbing a backup verify that the rows are really gone. |
| DNS rebinding against the UI | Host allowlist (IP addresses, localhost, this machine's names, configured hosts); other hosts get 421. |
| XSS, clickjacking | Strict Content-Security-Policy, `X-Frame-Options: DENY`, `nosniff`, `no-referrer`. The UI never renders HTML from data. |
| Secret leakage | NAS passwords and TOTP secrets are sealed (XChaCha20-Poly1305) with a master key that is never part of a backup. Secrets are write-only in the API and redacted in logs, snippets and the audit log. Only the root helper decrypts NAS passwords. CDN query strings are never logged or stored. |
| Privilege escalation | The service runs unprivileged and never holds `CAP_SYS_ADMIN`. NAS mounts are done by systemd on request of a separate root helper that re-validates every request and never trusts the database: it opens it read-only as a regular file (no links, FIFOs or devices) with an untrusted schema, touches only names derived from the target id, never follows links in the service-owned request directory, and runs sandboxed with a memory limit. |
| Resource exhaustion | Every cache, queue, map and upload is bounded; query timeouts, a size cap for the log database, connection caps per client and in total. |

Known residual risks:

- A LAN client can place content from a host it controls under Steam depot
  cache keys, because Steam requests are recognised by their User-Agent (as
  in the original LanCache). Steam verifies chunk checksums, so the effect is
  failed downloads for other clients (denial of service), not code execution.
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
      downloaded in the UI or API hold the configuration and the audit log
      (never accounts, password hashes, sessions or API tokens); file copies
      of `picache.db` hold everything. Backups "including secrets" are made
      only when needed.
- [ ] `keys/master.key` is backed up separately from the database (or the key
      comes from a systemd credential / Docker secret). A systemd credential
      is given to `picache-storage.service` too when host-apply mounts SMB
      shares with a stored password.
- [ ] The NAS account can access only the cache share. NFS exports are limited
      to PiCache's host.
- [ ] The query-log retention and client anonymisation settings match your
      privacy requirements.
- [ ] `lancache.nocacheClients` and `allowPrivateUpstreams` stay at their
      defaults (empty / off) unless you know you need them.

**Maintenance**

- [ ] PiCache is kept up to date. The CI runs `govulncheck` against the
      dependencies.
- [ ] The health page (**System → Health & about**) and the audit log are
      reviewed now and then.
