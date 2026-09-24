# Security

PiCache is a DNS resolver, an HTTP cache and a web application on your local
network. Its priorities are **security, simplicity, correctness,
performance**, in that order. This page summarises the threat model
(the binding details are in [ARCHITECTURE.md §6](ARCHITECTURE.md#6-security-model)),
gives a hardening checklist and explains how to report vulnerabilities.

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub's private
vulnerability reporting for this repository (**Security → Report a
vulnerability**). Do not open a public issue. Include the version
(`picache version`), the deployment type, and steps to reproduce. Never
include real passwords, tokens, setup tokens or NAS credentials; the database
and backups contain secrets and personal data (query logs).

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
| DNS cache poisoning | Encrypted upstreams (DoH/DoT) by default, random IDs and ports, the question is verified on every reply, in-flight deduplication, no client EDNS options forwarded. |
| Private reverse-DNS leaks | PTR/SOA/NS queries for private and special-use reverse zones never reach public upstreams. |
| Open HTTP proxy / SSRF via :80 | Only hosts of known LanCache services are served; unknown hosts get 403. Upstream addresses must be public unicast and not this machine; link-local (cloud metadata) is always refused, and redirects are re-checked. Non-canonical paths are never stored. Per-client fill limits and at most 16 ranges per request prevent WAN amplification. |
| Open TLS relay via :443 | TLS is never terminated. The SNI must match an enabled service, the same SSRF rules apply, and connections are capped and time out. |
| Hostile blocklists, cache-domains data or NAS content | Sizes, counts, names and patterns are validated; public-suffix patterns are rejected. Files are accessed through `os.Root`. Slice files carry validated, CRC-checked headers. |
| Web UI takeover on first start | One-time setup token (logged and stored with mode 0600, compared in constant time, first user created atomically), or provisioning from `PICACHE_ADMIN_PASSWORD_FILE`. |
| Password guessing | argon2id, throttling per client and per user (5 failures → 15 min lockout, TOTP failures count), a global attempt limit, optional TOTP with single-use codes. |
| Session theft, CSRF | 256-bit session tokens stored hashed; cookie `HttpOnly`, `SameSite=Strict`, `Secure` on HTTPS; idle and absolute timeouts; sessions re-checked on every request; cross-origin protection; JSON-only request bodies. |
| API token misuse | Tokens have scope `read` or `admin` and can never manage tokens, passwords, TOTP or sessions. Live streams re-check the token every 15 s. |
| DNS rebinding against the UI | Host allowlist (IP addresses, localhost, this machine's names, configured hosts); other hosts get 421. |
| XSS, clickjacking | Strict Content-Security-Policy, `X-Frame-Options: DENY`, `nosniff`, `no-referrer`. The UI never renders HTML from data. |
| Secret leakage | NAS passwords and TOTP secrets are sealed (XChaCha20-Poly1305) with a master key that is never part of a backup. Secrets are write-only in the API and redacted in logs, snippets and the audit log. Only the root helper decrypts NAS passwords. CDN query strings are never logged or stored. |
| Privilege escalation | The service runs unprivileged and never holds `CAP_SYS_ADMIN`. NAS mounts are done by systemd on request of a separate root helper that re-validates every request and never trusts the database. |
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
      `PICACHE_WEB_LISTEN=off`.
- [ ] API tokens use scope `read` wherever possible and have an expiry.
      Unused tokens and sessions are revoked.
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
      it, and PiCache falls back to plain DNS to its bootstrap servers while
      the clock is wrong.
- [ ] Host-apply (`--with-host-apply`) is installed only if you use it.
      Deleting `/etc/picache/host-apply.enabled` disables the root helper.

**Data and secrets**

- [ ] Backups of `picache.db` are stored like other sensitive data (they hold
      the configuration, user accounts and password hashes). Backups
      "including secrets" are made only when needed.
- [ ] `keys/master.key` is backed up separately from the database (or the key
      comes from a systemd credential / Docker secret).
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
