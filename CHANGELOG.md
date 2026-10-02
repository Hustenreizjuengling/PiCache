# Changelog

All notable changes to PiCache are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- Conditional forwarding: a target on another port of this machine (a local
  resolver such as `127.0.0.1#5335`) now also answers queries from this
  machine itself; the loop guard answered them with SERVFAIL. Ports 53, 853
  and 443 of this machine (PiCache itself) stay guarded.
- Blocklists: a list on PiCache's own address now fails with a message that
  says so and points to `file://` lists in `<data>/lists/local/`, instead
  of "private or local addresses are refused".
- Installer: `install.sh` and `get-picache.sh` treat a pre-release with a
  hyphen (`v1.1.0-beta-1`) as a release version, so the downgrade refusal
  covers it; going back with `PICACHE_ALLOW_DOWNGRADE=1` prints "went back
  from … to …" instead of "updated from".

### Changed

- Dependencies: SQLite driver modernc.org/sqlite 1.60.0, Vite 8.3.1 (web UI
  build only).

## [1.0.0] - 2026-09-30

The first stable release. The release candidates 1.0.0-rc.1 and 1.0.0-rc.2
were published for testing; everything they changed since 0.17.0 is listed
here, together with the fixes made after rc.2.

### Upgrade notes

- **Unraid:** the template now passes `--restart=unless-stopped
  --stop-timeout=30` in *Extra Parameters*. An existing container keeps its
  parameters: add both there (**Docker → picache → Edit**, then switch on
  *Advanced View* at the top right: the basic view does not show the
  field), or PiCache stays stopped after **System → Restart**, a restore,
  switching the DHCP server on or a crash, and the network has no DNS.

- **Upstreams with text after `#`:** 1.0.0 reads `host#port` as the port;
  0.17 and earlier ignored everything after `#`. An upstream, fallback,
  local PTR upstream, forwarder target or group upstream an earlier
  version saved with other text there (`tls://1.1.1.1#cloudflare-dns.com`,
  `9.9.9.9#dns.quad9.net`, `1.1.1.1:53#5353`) keeps its old meaning: it
  is used without that text (the log names each one) and stored so at the
  next save. New input with such text is still refused. A follower reads
  such entries in the configuration of a 0.16 or 0.17 primary the same way,
  so followers can still be upgraded first.
- **Docker without host networking, NAS templates:** the bridge, macvlan,
  TrueNAS and Synology compose files set `hostname: picache`, and the Unraid
  template passes `--hostname=picache`. Add it to a compose file of your
  own (Unraid: to *Extra Parameters* of an existing container). If the
  `tls` health check said after an update that the local CA does not cover
  a name of 12 hex digits (the container ID), do not create a new local
  CA: that name is no longer used.
- **Release candidates:** the refusal of an older release needs the
  `get-picache.sh` and `install.sh` of 1.0.0. Until 1.0.0 is the latest
  release, `releases/latest/download/get-picache.sh` is 0.17.0's, which
  installs any release: testers use the script of the release candidate
  (`releases/download/v1.0.0-rc.N/get-picache.sh`). Go back with the
  installed release's `get-picache.sh` and `--version`, not with the older
  release's installer.

### Added

- **Guide "Moving from another DNS filter"** (docs/GUIDES.md): how to
  bring lists, own rules (adblock-style and regular-expression lists),
  local records (hosts files), conditional forwarders, clients and groups
  and DHCP reservations over with PiCache's importers, with the
  conversions of the usual text formats and the order of the switch-over.

### Changed

- **README** rewritten for 1.0: features, requirements, installation,
  network set-up, data, backups, updates and going back, development,
  troubleshooting, security and known limitations, with current
  screenshots.

- **Prometheus metrics:** `/metrics` accepts an API token of scope read;
  admin tokens keep working, sessions and sync tokens are still refused.
  The scrape configuration no longer needs a token that can change the
  settings, and the UI and the guide suggest a read-only token.
- **Installing an older release:** `install.sh` and `get-picache.sh`
  refuse a release older than the installed one, because the older version
  cannot open a database the newer one migrated and would not start. The
  message gives the safe order: unless the upgrade notes say that the older
  version opens the database, stop PiCache and put the copy named after it
  back first, then install with `PICACHE_ALLOW_DOWNGRADE=1` (as for the
  Debian package) and the options of the run (`--without-updater` is not
  kept between runs). The Debian package's steps after a failed upgrade
  use the same order.

- **Query-log searches** read the log in windows of about 200 000 rows and
  end a page after 4 s with the matches found so far, `partial: true` and
  a cursor where the search stopped (the web UI says so; the next page
  searches further back). A filter that matches few rows (a domain that
  does not occur, a rare type or response code) failed with 503 after
  10 s on a log of millions of rows, and a filtered export broke off.
- **Clearing the query log** answers at once: the rows are gone from every
  read and are deleted in the background in chunks. On a log of 7–8
  million rows the clear failed with 500 after about 40 s, kept the rows
  and dropped query events meanwhile.
- **`picache db check`** also decodes the settings document; `picache db
  salvage` shows `?` as the lost rows of a table it cannot read at all
  (it reported 1000000 for an empty table).
- **DNS over TCP, DoT and DoH connections:** at the limit of 1024, the
  client holding the most connections gives up its oldest one for a
  client holding fewer, and this machine may open 64 more. One host with
  many addresses could hold every connection and keep everyone else (the
  host's own resolver included) from answers that need TCP.
- **In a container, `picache serve` as root refuses an empty
  `PICACHE_RUN_AS`** ("must be numeric non-root uid:gid", exit code 2);
  before, PiCache kept running as root.

### Fixed

- **DNS over TCP:** a reply that cannot be written within 10 s now closes
  the connection. A client that stopped reading held its connection, a
  handler and an in-flight slot until it closed the socket (the DNS library
  never applies a write timeout).
- **DoH upstreams:** an HTTP/2 connection that died silently (lost NAT
  state, a router reboot) is pinged after 10 s without a frame and replaced
  within about 15 s. Before, queries timed out on it until cancelled
  requests had used up the server's stream limit, for minutes on a quiet
  network.
- **DoH upstreams:** every dial ends with the attempt timeout, and an
  upstream gets at most 4 connections (dials included). A black-holed
  upstream left a socket and a goroutine behind every request for about two
  minutes, so memory grew with the query rate.
- **DoH and DoT upstreams:** queries no longer fail with "unexpected EOF",
  "EOF" or "connection reset" when the server closes a connection just as
  a query is sent on it, or cuts the TLS handshake of a new connection.
  dns.quad9.net closes its HTTP/2 connections after seconds to minutes
  without announcing it (no GOAWAY) and closes or resets a share of new
  TLS handshakes, on its DoH and DoT ports; the query of that moment
  failed (about 0.2–0.6 % of the queries), went to the fallback DNS, and
  the health check warned "fallback DNS in use". Such a query is now sent
  once more, at once, over a new connection and within the same time
  limit (a closed reused connection also for HTTP/3 upstreams); at most
  once per query. A reply with an HTTP status, a malformed reply, a
  certificate error, a timeout or a refused connection is never a reason
  to send it again.
- **Health check `upstreams`:** a single query answered by the fallback DNS
  made the check warn "fallback DNS in use" for 5 minutes, several times a
  day with the default upstreams. It now warns once the fallback answered
  at least 3 queries within 5 minutes, and still at once while no default
  upstream is healthy.
- **Upstreams with the port written as `#port`:** `10.0.0.53#5353`, the way
  other DNS filters write local resolvers such as `127.0.0.1#5335`, was
  accepted but asked on port 53, as upstream, fallback, group upstream and
  conditional-forwarder target (the forwarder import too). Plain, `tcp://`,
  `tls://` and `quic://` upstreams now take the port after `#` like after
  `:`, and are kept and shown as typed. An upstream an earlier version
  saved this way reaches the intended port after the update, without a
  change. Other text after `#` (a port after `:` as well, a name, 0 or a
  number over 65535) is refused ("write the port as host:port"); an entry
  an earlier version saved with such text is ignored with a warning in the
  log until it is corrected. DoH URLs still refuse `#`. Support bundles
  reduce the address of a `#port` upstream like that of a `:port` one
  (before, `203.0.113.53#5353` became `*.113.53#5353`).
- **Stale clock:** a host without a real-time clock whose time was
  restored from its last shutdown could not reach any encrypted upstream
  once their certificates had been renewed meanwhile, so every name, the
  NTP servers' included, failed. While the host clock is not synchronised,
  PiCache now checks the upstreams' certificates at the start of the renewed
  certificate's validity (its chain must verify; an expired certificate and
  a synchronised clock change nothing) and logs a warning.
- **DNSSEC:** a device's share of the chain lookups now waits for its turn
  within the validation's time, like the global limit, instead of failing
  at once (25 per second with a burst of 200 instead of 20 and 100), so a
  cold burst of new zones from one device no longer gets SERVFAIL. The
  share is keyed by the device's MAC address when it is known, so IPv6
  privacy addresses or extra addresses no longer get a share each. A
  refused lookup is logged once per device with the `dns.rateLimitExempt`
  hint instead of a warning per zone.
- **Encrypted upstreams by name:** when the first bootstrap address of a
  DoT, DoH or DoQ upstream drops the packets silently (a broken IPv6 path
  with IPv6 preferred for the bootstrap, a filtered anycast address), every
  connection attempt gives the next address its share of the time. Before,
  an attempt could spend all of it on the first address, so a DoT or DoQ
  upstream stayed down as long as that address did not answer.
- **Fallback upstreams:** while every default upstream is unhealthy, the
  fallbacks answer after 500 ms instead of after the whole 3 s attempt; the
  default upstreams are still asked, so their recovery ends this at once.
- Flaky tests on Windows (the DoT answered counter, a loopback RTT of 0)
  and data races in test fakes; CI runs the DNS and netutil tests under the
  race detector.
- **Restarts:** DNS stays up until the web UI and the download cache
  have stopped, and open event streams end at once. An open application
  log held every stop for 12 s with DNS already down, longer than
  `docker stop` waits by default (10 s), so PiCache was killed before it
  wrote its last log batch. A download on the cache port is now cut after
  3 s (its client resumes it) instead of holding the stop for 12 s.
- **Admin password file:** a `PICACHE_ADMIN_PASSWORD_FILE` left in
  `picache.env` (or in the compose file) after its file was deleted no
  longer stops PiCache once an account exists; it logs a warning instead.
  Before the first account exists a missing file still stops the start.
- **Command line and `picache.env`:** of a variable listed twice the CLI
  now uses the last line, like systemd. `picache healthcheck` and
  `sudo picache update` could probe another DNS listener than the service
  bound, report it unhealthy and roll back a good update.
- **Memory limit:** `GOMEMLIMIT` and the entry budget of the blocklists
  follow a `MemoryMax=` of `picache.service`, limits of the cgroups above
  it and cgroup v1 limits (for example Synology's Container Manager), not
  only the limit a container sees at the root of its cgroup namespace.
- **Start errors:** a `picache.db` that PiCache cannot write now names the
  cause (the file or the data directory belongs to another user, with the
  `chown` command, or the file system is read-only), and a damaged one
  points to `picache db check`; neither is called a "pre-upgrade backup"
  error any more.
- **Downgrades without the copy:** from this release on, an older version
  started on a newer `picache.db` refuses before it copies the database
  under its own name or records its version. Before, the next start of the
  newer version saved the migrated database as the older version's copy
  and could prune the genuine one. Versions before 1.0.0 still behave that
  way: put the copy back before you start one of them (DEPLOYMENT "Going
  back to an earlier version").
- **Full data disk:** saving a setting answers 503 "the data disk is full"
  instead of "internal error (see server log)".
- **Broken `logs.db`:** only the newest `logs.db.broken-*` copy is kept;
  each earlier corruption or downgrade left a copy of up to 2 GiB.
- **Follower sync:** a reload that fails after the sync was committed fails
  the run, so the next run applies the export again, including lists whose
  address, kind or format changed (reloaded, not kept with the old ones)
  and the downloaded copies of lists that run removed. Before, the follower
  answered with the previous configuration while its database held the new
  one, and the sync was reported as successful.
- **`get-picache.sh`:** it works on hosts that mount `/tmp` noexec (it runs
  the downloaded program below `/var/tmp` or `$TMPDIR`), and a noexec
  directory is named as the cause instead of "the downloaded binary does
  not run on this machine".
- **System → Updates:** the notice shown while a pre-release runs on the
  stable channel pointed to the switch "Include pre-releases", which was
  replaced by the update channel in 0.15.0; it now says to choose the
  channel Beta.
- **Documentation:** scheduled backups are off by default, and
  `keys/master.key` and `tls/` (the local CA your devices trust) belong
  next to every backup and move (DEPLOYMENT "Backup and restore");
  `picache.prev` exists only after an update from the web UI or
  `sudo picache update`, so check its version before going back with it;
  the update channel and the install proxy are on **System → Updates**;
  the maintenance commands ignore `PICACHE_ADMIN_PASSWORD_FILE`; the
  development command no longer opens DoT on all interfaces; the compose
  files no longer claim that a container cannot read the host's clock
  state.

- **Debian package:** `apt purge picache` (or a purge of all removed
  packages) after switching to `install.sh` deleted `/etc/picache`, the
  data (database, master key, local CA, backups) and tried to delete the
  account of the running `install.sh` installation. While `install.sh`'s
  binary or unit exists, the purge now deletes nothing of it and only
  forgets the package's unit state (without deleting the enable links the
  units share).
- **Purge:** a failed `userdel` (the account still in use) is warned about
  and no longer followed by "Deleted the account picache".
- **Debian package downgrade:** the installed package refuses an older
  one first, with the safe order (stop PiCache, put the database copy
  back, then install with `PICACHE_ALLOW_DOWNGRADE=1`). dpkg then runs the
  older package's scripts, whose preinst refuses as well; one before 1.0.0
  offers `PICACHE_ALLOW_DOWNGRADE=1` there as an alternative, which the
  first message and DEPLOYMENT say not to follow.
- **`get-picache.sh` without `--version`** on a host whose installed
  release candidate is newer than the latest release says "nothing to do"
  instead of the steps to go back to that older release.
- **Upgrade from 0.16/0.17:** upstreams saved with text after `#` were
  dropped, so a default set of only such upstreams was empty and every
  query got SERVFAIL, forwarders and group resolvers failed closed and
  every settings save was refused. The health check `upstreams` now fails
  when no configured upstream can be used ("no usable upstream DNS server
  is configured"); it said ok.
- **Follower sync from a 0.16/0.17 primary:** upstreams, fallbacks, group
  upstreams and forwarder targets the primary sends with text after `#`
  keep their old meaning, as stored ones do, and are logged ("the primary
  sent an upstream with text after "#" …"). The follower refused the
  primary's DNS settings, and with them every other synced section.
- **Pre-upgrade copies:** a version before 1.0.0 started on a newer
  database records itself before it fails; the next start of the newer
  version named its copy after that older version, which cannot open it,
  and could prune the genuine copy. The copy is now named after the version
  whose schema the database has, and pruning keeps the newest copy of every
  schema. When the older version already copied the database under the
  newer version's name, the newer version adds no second copy of it, which
  used up the three places the older version keeps. That older version
  still prunes by its own rule before it fails: DEPLOYMENT now says to copy
  the needed file out of `backups/` before going back.
- **Restore on another machine:** a full or `storage` restore keeps this
  machine's own local cache store, and a `picache.db` copied from another
  machine adopts the store in the cache directory when the copied one was
  never used here. The download cache stayed offline ("a different cache
  store … was found") until the store was adopted by hand.
- **Docker:** the local CA and its certificate no longer name the
  container's default host name (its ID), which changed with every
  recreated container, so the `tls` check warned after every update.
- **`picache restore`** in a container says `docker restart <container>`
  instead of `systemctl restart picache`.
- **`picache query`** prints the answer records with spaces instead of
  escaped tabs (`\u0009`).
- **Full data disk:** signed-in browsers and API tokens failed with 503
  on every request, the health page included, once recording their last
  use failed; that record is now best effort.
- **Blocklists after a restart:** DNS answered before the cached lists were
  compiled, so for up to a few seconds after every start listed names
  (the parental categories included) were answered unfiltered. The DNS,
  DoT, DoH and web listeners now serve once they are compiled (at most
  30 s later; the queries wait in the bound sockets).
- **A missing or empty `picache.db`** in the data directory of an existing
  installation stops the start with the way back; PiCache silently started
  a new installation with the default filtering and first-run setup.
  Deleting `instance-id` starts a new installation on purpose.
- **A damaged `logs.db`** (damaged inside, so it still opens) made query
  log reads fail with 500 while the health check said ok. The damage is
  reported by the health check `logs`, and the next start checks the file
  and moves it aside. Events of failed writes are no longer reported as
  "dropped under load".
- **A `logs.db` PiCache may not write** (owned by root after a copy) was
  moved aside as broken and later deleted; it is kept, logging is off and
  the cause is named, as for `picache.db`.
- **Clock set far ahead:** the web certificate issued meanwhile (not valid
  until then) is replaced when the clock is right again, with a new local
  CA if that is not valid yet either (the health check `tls` warns
  meanwhile); after a clock jump (more than an hour ahead or back while
  PiCache runs, or the log last written after the time of the start) the
  retention waits until the host clock is synchronised, at most a day,
  instead of deleting the query log and the statistics, while a clock that
  is only reported as not synchronised (no NTP client, Docker Desktop)
  delays nothing; lists checked "in the future" are updated again; and the
  health check `upstreams` names the clock when every upstream fails on its
  certificate's validity.
- **Blocklist with an unreadable cached copy** (damaged, another owner)
  stayed empty while the server answered 304 Not Modified. It is now
  downloaded again in full and replaced, and the health check names the
  stored copy instead of the internet connection.
- **Damaged settings document:** the start error names the way back
  (`picache restore <backup file>`).
- **Web certificate without network at the start:** its LAN addresses are
  kept, and an address that appears is added at the next minute instead
  of after up to an hour.
- **Replaced master key:** a new `keys/master.key` created while the
  configuration holds secrets sealed with the previous one is logged as an
  error, and the health check `master-key` warns while a stored secret
  cannot be decrypted with the new key (it tries each one, so saving a
  notification channel or NAS target without entering its secret again
  does not end the warning).
- **Ownership error of a named volume** names the command for a volume
  (`docker run --rm -v <volume>:/data alpine chown -R …`), not only the
  one for a bind mount.
- **Query-log export:** a failure on the server is audited as
  `truncated: "error"`, no longer as `"disconnected"`.
- **DoH:** the DNS access list is checked before the request is read, so a
  source outside it gets 403, never 400 or 413.
- **Docs:** the README's Docker commands work with `picache-deploy.tar.gz`
  (`cd deploy/docker`); a damaged `picache.db` in Docker is checked with a
  one-off container (`docker exec` cannot run in a restarting container);
  going back to an older release, the downgrade refusal of the installers
  and the recovery after a restore on another machine are described as
  they work; the 0.17.0 upgrade notes give the safe order for going back
  with the Debian package (stop, restore the copy, then install).

- **`host#port` upstreams are saved as `host:port`** (upstreams, fallbacks,
  local PTR upstreams, forwarder targets and their import, group upstreams,
  the follower sync; IPv6 in brackets, the scheme kept). 0.17.0 ignores
  `#port`: after going back, `127.0.0.1#5335` saved by a release candidate
  asked `127.0.0.1:53`, possibly PiCache itself, without a warning. An
  entry a release candidate saved as typed keeps working and is saved as
  `host:port` at the next save; save it once before going back to 0.17.0.
- **A signed-in browser stays signed in on a full data disk** while it is
  used: the idle time counts from the last use kept in memory while it
  cannot be written, instead of expiring the session after the idle time
  however often it was used. The absolute limit still applies.
- **Forwarder targets and group upstreams** that an earlier version saved
  with other text after `#` are logged when they are loaded (once per
  forwarder or group, naming it), as the upgrade notes promise; only the
  DNS settings were.
- **Signing in on a full data disk** says what is wrong: "cannot sign in:
  PiCache's data disk is full (a sign-in stores a session); free space on
  the host — signed-in browsers and API tokens keep working" (the setup
  has its own message).
- **A damaged `logs.db`:** query-log and statistics reads that hit a
  damaged page answer 503 "logs.db is damaged: restart PiCache; it moves
  the file aside" instead of 500 "internal error", and the health check
  `logs` warns at once.
- **Pre-upgrade copy after going back to 0.17.x:** upgrading again made no
  copy and warned that the previous version had not migrated the database,
  so the changes made under 0.17.x were in no copy. 0.17.x opens the
  database of 1.0.0 (no schema step): the copy is named after it, logged at
  INFO.
- **`install.sh`** prints "PiCache is updated and running" (with the
  versions) instead of the setup token and the first-install steps when
  it updates an installation that is set up; the Debian package no longer
  names the setup token then.
- **Local PTR upstreams** with text after `#` get the message of the
  upstream syntax ("write the port as host:port") instead of "must be a
  plain DNS server IP".

### Security

- **Encrypted upstreams:** queries to DoT, DoH, DoQ and HTTP/3 upstreams
  are padded to multiples of 128 bytes (EDNS Padding, RFC 8467), so their
  length no longer reveals the length of the name, and resolvers pad their
  replies too.
- SECURITY and ARCHITECTURE now name the residual risk of DNS rebinding to
  global IPv6 addresses of the LAN, and how to cover a static prefix.
- **Web UI connections:** the HTTP and HTTPS web listeners accept at
  most 64 connections per client address, 256 for the IPv6 addresses of
  one /64 of the LAN together, and 1024 in total (trusted reverse proxies
  count only toward the total), like the other listeners; this machine may
  open 64 more, so a host that fills the total cannot fail PiCache's health
  check, mark the container unhealthy or roll back an update. One host in
  the allowed networks could open idle connections until the kernel
  stopped PiCache for lack of memory (about 1 GB for 25 000 idle HTTP/2
  connections), and DNS with it.
- **`get-picache.sh --version`:** the downloaded binary must report the
  release asked for. The signature holds for every release, so a mirror
  given with `PICACHE_RELEASE_BASE` could serve an older signed release
  under a newer version and the script installed it; without `--version`
  it names the installed version next to the new one.
- **Sign-in throttling:** attempts in flight now count as failures, so
  concurrent sign-ins and password confirmations get no more password
  checks than the same requests one after another. Before, parallel
  requests passed the check together: ten from one address got ten
  guesses where the lockout allows five, and a delayed username several
  per delay.

## [0.17.0] - 2026-09-30

### Upgrade notes

- **Databases:** `picache.db` gets settings migration 7 (`dns.dnssecMode`
  is set from `dns.dnssec`: `true` → `passthrough`, else `off`) and dns
  migration 4 (the column `validate` of `dns_forwarders`, `false` for
  every forwarder); `logs.db` gets logs migration 6 (the column
  `dnssec_status` of `logs_queries`; older rows have no status). Upgraded
  installations keep their behaviour: local validation is on only in new
  installations.
- **Downgrade:** 0.16 refuses the migrated `picache.db` (settings v7, dns
  v4): go back with the copy 0.17 made at its first start (the automatic
  rollback uses it; Docker users restore it before starting the older
  image; `.deb` users stop PiCache, restore the copy, then install the
  older package with `PICACHE_ALLOW_DOWNGRADE=1`; corrected in 1.0.0: this
  note first gave the unsafe order, installing before restoring). The
  DNSSEC mode and the forwarders' validate flags set since the upgrade are
  lost. 0.16 sets the new `logs.db` aside (`logs.db.broken-<timestamp>`).
  Every settings document 0.17 writes keeps `dns.dnssec` with its old
  meaning.
- **Follower sync:** upgrade the followers first; a 0.16 follower refuses
  the export of a 0.17 primary (newer schema). A 0.17 follower of a 0.16
  primary maps `dns.dnssec` to the mode.
- **API:** `dns.dnssecMode` (`off`, `passthrough`, `validate`) replaces
  `dns.dnssec`, which stays as an alias (`true` = `passthrough`, or keeps
  `validate`; `false` = `off`; a `dnssec` changed in the same write that
  contradicts a changed `dnssecMode` is refused with 400, otherwise
  `dnssec` is rewritten from the mode); `GET /settings/defaults` reports
  `validate`. New: `POST /dns/dnssec/test`, `GET /stats/dnssec`, the
  query log filter and export parameter `dnssecStatus`,
  `QueryEvent.dnssecStatus`, `validate` of conditional forwarders,
  `dnssecStatus`, `dnssecReason` and `dnssecEde` of `POST /dns/lookup`,
  `dnssec` of `GET /dns/stats`, the DNSSEC members of
  `GET /dns/upstreams` (validate mode), the health check `dnssec` and the
  metric `picache_dns_dnssec_total{status}`. The **CSV export gains a last
  column** `dnssecStatus` after `dnsClientId` (parsers that read the
  header are not affected). `QueryEvent.dnssec`, the `dnssec` filter and
  the CSV column `dnssec` (the AD flag sent to the device) are unchanged.
- **Units, `install.sh` and the packages:** unchanged. The mode `validate`
  needs time synchronisation on the host (DEPLOYMENT "DNSSEC").

### Added

- **Local DNSSEC validation** (`dns.dnssecMode: validate`, **DNS settings
  → DNSSEC**): PiCache verifies the answers of the default upstreams, the
  fallbacks and the group resolvers along the chain of trust from the
  built-in root trust anchors (KSK-2017 and KSK-2024; RSA/SHA-256 and
  SHA-512, ECDSA P-256 and P-384, Ed25519; NSEC and NSEC3), answers bogus
  data with SERVFAIL and an extended DNS error (a device with the CD flag
  gets the data without AD), removes records that are not part of the
  answer, and sets the AD flag only for answers it verified. It uses only
  the DNSSEC primitives of `miekg/dns` and the standard library (no new
  dependency), bounds every validation (KeyTrap and NSEC3 limits),
  caches validated keys per upstream route, never fails open silently
  (upstreams without DNSSEC data, stale trust anchors and a wrong clock
  are reported, their answers passed on without AD) and never locks a
  Raspberry Pi without RTC out (the date checks wait for time
  synchronisation). **New installations start with `validate`**,
  measured before the release: a cold validation of a signed root → TLD →
  SLD chain with an NSEC3 proof takes 15.6–15.9 ms under armv7 emulation
  (qemu, slower than a Raspberry Pi 4; the limit was 20 ms; 0.29 ms on
  x86-64), a cold name below a cached TLD costs exactly 2 extra upstream
  queries, 10 000 validated zones add 4.3 MiB of heap (limit 16 MiB), and
  a response-cache hit verifies nothing.
- **Validate DNSSEC** for conditional forwarders with their own DNS
  servers outside the locally served zones (`validate`); chain lookups go
  only to that forwarder's servers.
- **DNSSEC status** of every validated answer (`secure`, `insecure`,
  `bogus`, `indeterminate`) in the query log (a shield marks secure and
  bogus rows), its filter, the live feed, the export (`--dnssec-status` for
  `picache logs export`), the statistics (`GET /stats/dnssec`, a DNSSEC
  list in the overview's DNS band), `picache query`, the domain tester and
  its trace. In the mode `validate` the upstream lists of the DNS settings
  and the conditional forwarders show whether each upstream returns DNSSEC
  data.
- **Test DNSSEC** (`POST /dns/dnssec/test`): probes the default upstreams
  and checks four fixed names (`example.com`, `google.com`,
  `dnssec-failed.org`, `sigfail.ippacket.stream`) in every mode, so it
  shows whether `validate` works before switching.
- The health check **`dnssec`** (validate mode): trust anchors that no
  longer match, suspended time checks, upstreams without DNSSEC data, a new
  root key, many bogus answers.

### Changed

- Replies **echo the client's CD bit** (RFC 4035 3.2.2), in every mode.
- **DNS64** no longer synthesises AAAA records for a query with DO and CD
  set (RFC 6147 5.5), in every mode: the AAAA answer is returned as it is.
- In the mode `validate` the upstreams' AD flag is discarded on every
  route (also for forwarders without validation, the router and the local
  PTR servers), so only PiCache's own verdict sets AD.
- **Unbound guide:** PiCache uses the mode `passthrough` in front of a
  validating Unbound.

## [0.16.1] - 2026-09-30

### Fixed

- Download cache: concurrent first requests for an object on a host without
  range support now always share one upstream download (#3).
- Local DNS: an SRV, MX, SVCB or HTTPS record with a number out of range is
  no longer answered with a wrapped value; interface index and MTU are read
  safely on 32-bit hosts (CodeQL findings).

## [0.16.0] - 2026-09-29

### Upgrade notes

- **Databases:** no schema step; 0.15.0 opens the databases of this version.
  The new setting `web.onboardingDone` loads as `true` on upgraded
  installations (no getting-started checklist; a fresh installation shows
  it).
- **Units:** only comments change; the new binary runs with the 0.15.0
  units.
- **Architectures:** armv7 installations stay on the armv7 build (the
  updater reads the ARM version from the running binary). New builds for
  armv6, 386 and riscv64 (best effort).
- **API:** `GET /network/check` gains `self.dynamic4` and `requester`;
  `GET /system/update` reports the mode `package` with `package`;
  `POST /system/update/apply` answers 409 in that mode; new
  `GET /api/v1/openapi.json`. The batch 409 of
  `POST /filter/lists/batch` and the health check `blocklists` name the
  entry budget of the host, which now follows its memory.

### Added

- **Debian packages** `picache_<version>_<arch>.deb` for amd64, arm64, armhf
  (the armv6 build), i386 and riscv64 in every release. They install
  `/usr/bin/picache` and the units in `/usr/lib/systemd/system`, never the
  update helper: updates are installed with apt (the web UI and
  `picache update` show the steps). The package refuses to install over
  `install.sh` and to downgrade without `PICACHE_ALLOW_DOWNGRADE=1`, never
  prints the setup token, and `apt purge` deletes only the default paths;
  `install.sh` and `get-picache.sh` refuse to touch a package installation.
  No apt repository yet; verify the package against the signed
  `SHA256SUMS` first.
- **More architectures:** `picache-linux-armv6` (Pi Zero W, Pi 1),
  `picache-linux-386` (SSE2) and `picache-linux-riscv64`, best effort (built,
  vetted and started under emulation in CI); container images also for
  linux/riscv64.
- **More distributions** for `install.sh` and `get-picache.sh`: Ubuntu
  22.04+, Fedora, RHEL/Alma/Rocky 9+, Arch, openSUSE Tumbleweed and Leap 16
  besides Debian 12/13; systemd 247 or later is required. SELinux labels
  are restored with `restorecon`; while firewalld or ufw is active the
  installer prints the commands for PiCache's ports (it never changes the
  firewall); missing mount programs are named with the command of the
  distribution.
- **NAS and macvlan:** `deploy/docker/docker-compose.macvlan.yml` (an own LAN
  address) and templates for Unraid, TrueNAS SCALE and Synology with the
  full hardening and a non-root `PICACHE_RUN_AS`.
- **Translations:** every language except English is loaded when chosen;
  `npm run check` checks keys, plural forms, placeholders and commands of
  every language, and `docs/TRANSLATING.md` describes how to add one.
- **Getting started:** a checklist on the Overview for new installations
  (upstreams and lists, a fixed address, the router, a test from a device),
  based on `self.dynamic4` and `requester` of `GET /network/check`; step-by-
  step guides for single devices (**DNS → Network check → Set up a device**,
  `docs/DEVICES.md`).
- **Settings search** in the header: a combobox over every settings page and
  option, matched in the browser; a hit opens the page at the option.
- **Guides:** `docs/ROUTERS.md` (OPNsense, pfSense, OpenWrt, ASUS, TP-Link,
  UniFi, Speedport, Vodafone Station) and `docs/GUIDES.md` (Unbound as a
  local recursive resolver, filtering away from home with WireGuard or
  Tailscale, Home Assistant, firewall rules on the host and on the router).
- **OpenAPI 3.1** description of the whole API at `GET /api/v1/openapi.json`
  (read permission), checked against the route registry and the settings.
- **Docker Hub mirror** of the release images (optional, copied by digest
  from GHCR) in the release workflow.

### Changed

- The **entry budget** of the blocklists follows the memory of the host
  (the container's limit, else the machine's nominal memory):
  4 000 000 entries from 1 GiB, less below (2 000 000 with 512 MB), at least
  500 000. The minimum is 512 MB of memory.
- `install.sh` runs everything from `main` on its last line (the Debian
  package's maintainer scripts share its functions); the purge also deletes
  the log directory `/var/log/picache`, and keeps and locks the `picache`
  account while kept directories hold its files. Raspberry Pi OS 32-bit
  (`ID=raspbian`) counts as Debian.
- `get-picache.sh` finds the CA bundle of every supported distribution,
  installs missing tools with `apt-get`, `dnf` or `zypper`, needs OpenSSL 3
  and explains a failed signature check in FIPS mode.
- The hints for missing NFS and SMB clients name the package of every
  distribution family.

### Declined

- **Nightly container images:** a scheduled job would need
  `packages: write`, with which it could overwrite `latest` and `X.Y`.
  Build `main` locally with `docker compose up -d --build` instead.

## [0.15.0] - 2026-09-27

### Upgrade notes

- **Databases:** `picache.db` gets auth migration 3 (`auth_tokens` is
  rebuilt for the new scope `sync`; tokens keep their scopes) and settings
  migration 6 (the table `settings_secrets` for the sealed sync token and
  proxy password; `updates.channel` is set from `includePrereleases`:
  `true` → `beta`, else `stable`). The new sections `clients`, `sync`,
  `network`, `ntp` and `logs.seenRetentionDays` load with their defaults.
  `logs.db` has no step.
- **Downgrade:** 0.14 refuses the migrated `picache.db`; going back needs
  the copy made before the upgrade (the automatic rollback uses it). 0.14
  ignores listeners saved in the web UI and binds the `PICACHE_*_LISTEN`
  values and defaults again.
- **Units:** `picache.service` gets `LogsDirectory=picache`,
  `LogsDirectoryMode=0750` and `SystemCallFilter=adjtimex` and loses
  `ProtectClock=yes` (its own filter refuses reading the clock state; the
  clock still cannot be set: `CAP_SYS_TIME` is not in the bounding set and
  the setting calls stay filtered). The one-line installer or an update
  from the web UI installs them; until then a log file below
  `/var/log/picache` cannot be opened and the NTP server answers
  unsynchronised. Checked under systemd 252 (Debian 12) and 257 (Debian 13):
  with the new unit a read-only `adjtimex` succeeds while a setting
  `adjtimex` and `clock_settime` fail; with `ProtectClock=yes` the read
  fails too.
- **API:** `POST /tokens` accepts the scope `sync`; `GET /auth/status`
  gains `syncedSections`; `GET /system/update` gains `channel`,
  `nightlyAllowed` and `installProxy`; `GET /system/log` gains `sinks`;
  `POST /system/restore` takes `?sections=` and answers `sections`;
  `PUT /settings` and `PATCH /settings/{section}` take `?dryRun=true`, and
  PATCH accepts `clients`, `sync`, `network` and `ntp`;
  `clients.Known`, `api.NetworkDevice`, `dhcp.Lease` and
  `dhcp.StaticLease` gain `vendor` and `macRandomized`; the support
  bundle's `listeners.json` gains `effective`. New log components `ntp`
  and `sync`.

### Added

- **Vendor of a MAC address:** the IEEE MA-L, MA-M and MA-S registries are
  embedded (`internal/oui`, about 1 MiB, refreshed with `make oui`); the
  network check, the seen clients, DHCP leases and reservations show the
  vendor, or "Private address (randomised)" for locally administered MACs.
  The IEEE does not restrict the redistribution of the listing, so the
  table is embedded (THIRD_PARTY_NOTICES.md).
- **Seen clients:** forget an address or a device (`DELETE /clients/known`)
  or all seen data (`POST /clients/known/flush`); retention
  `logs.seenRetentionDays` (7–365, default 30); the interface, vendor and
  network owner of each address.
- **Identify clients by interface** (`iface:<name>`: the interface the route
  to the source leaves by; for guest VLANs and VPNs) **and by host name**
  (`host:<name>`: the device's DHCP, PTR or hosts-file name; spoofable,
  opt-in per identifier).
- **Name sources** (`clients.nameSources`): DHCP lease names and PTR names
  can be switched off, `/etc/hosts` and WHOIS (RDAP, the owner of a public
  network; sends the /24 or /48 out) switched on.
- **Network interfaces** (`GET /network/interfaces`): state, speed, MTU,
  addresses, networks, gateways and counters per interface.
- **Command line:** `picache status [--watch]`, `pause`, `resume`,
  `explain`, `lists update`, `allow`, `deny`, `query`, `config get|set|apply`
  (with `--dry-run`), `restore` and `listeners --reset`.
- **Declarative configuration:** `?dryRun=true` on the settings routes,
  `picache config`, and `PICACHE_INITIAL_CONFIG` (a settings document applied
  once at the first start).
- **Log file and syslog** (`PICACHE_LOG_FILE`: a `<name>.log` file below
  `/var/log/picache`, directly in the data directory or below its `logs/`,
  rotated at 10 MiB with 5 compressed generations; `PICACHE_LOG_SYSLOG`,
  RFC 5424 over UDP or TCP) and the health check `logging`.
- **Profiling** (`PICACHE_PPROF=on`): Go profiles on `/debug/pprof/` for
  admins on the PiCache host.
- **Listeners in the web UI** (System → Network; `GET`/`PUT
  /system/listeners`): saved in the data directory and applied at the next
  start; a saved listener that cannot be bound falls back to its variable or
  default instead of stopping PiCache.
- **Partial restore:** restore selected sections of a backup (settings,
  clients and groups, lists and rules, local DNS, parental controls, DHCP,
  download-cache services, notifications, storage), from the web UI or with
  `picache restore`.
- **Follower sync:** a second PiCache pulls clients and groups, lists and
  rules, local DNS, parental controls and the DNS filtering settings from a
  primary (`GET /system/export` with the new token scope `sync`; System →
  Sync), applies them live and keeps them read-only; health check `sync`.
- **Update channels** (`updates.channel`: stable, beta, nightly) and
  **nightly builds** of `main`, signed with a separate key
  (`docs/nightly-key.pem`) and installed by the update helper only on hosts
  with `install.sh --nightly`.
- **NTP server** (`PICACHE_NTP_LISTEN`, `ntp.enabled`): SNTP answers from the
  host clock, behind the DNS access list and a rate limit; health check
  `ntp`.
- **Outbound proxy** (`network.proxy`, `network.proxyFor`) for list
  downloads, the release check and notifications (HTTP CONNECT or SOCKS5
  tunnels to the checked addresses), and `PICACHE_UPDATE_PROXY` for the
  update helper.

### Changed

- **Dependencies:** none added; `golang.org/x/net/proxy` (part of the
  existing `golang.org/x/net`) is used for SOCKS5.
- **Settings:** `updates.includePrereleases` is derived from
  `updates.channel`.
- **Healthcheck and CLI** find the web and DNS listeners also when they
  were saved in the web UI.

## [0.14.0] - 2026-09-27

### Upgrade notes

- **Databases:** `picache.db` has no schema step. The settings keep their
  version: `dns.plainDns` (`true`) and `dns.encrypted`
  (`{dot:false, doh:false, serverName:""}`) load with these defaults.
  ClientIDs of clients are rows of `client_identifiers` with the kind
  `clientid` and the value `clientid:<id>`. `logs.db` gets logs migration 5,
  which adds the column `dns_client_id` to `logs_queries` (no table
  rewrite). The ClientIDs devices sent stay in memory.
- **Visible after the upgrade:** `:853` is bound (`PICACHE_DOT_LISTEN`;
  connections are closed at accept while DoT is off; a port clash is logged
  at the start and reported by the health check only while DoT is on).
  Docker bridge users publish `853:853/tcp` to use DoT (the bridge compose
  file does). Installations with `PICACHE_WEB_TLS_LISTEN=off` now get the
  local CA at the first start (the DoT listener is a TLS listener).
  Nothing else changes until DoT or DoH is switched on.
- **API:** `upstream.UpstreamStat` gains `name` (the display name; `upstream`
  stays the configured entry, a DNS stamp shows as `sdns:<protocol>:<host>`
  in `name`), `logs.QueryEvent` gains `dnsClientId` and the `protocol`
  values `dot` and `doh`, the CSV export a column `dnsClientId` after `ecs`.
  The 409 without a TLS listener (`PUT /system/tls`,
  `POST /system/tls/local-ca`) now reads "no TLS listener:
  PICACHE_WEB_TLS_LISTEN, PICACHE_DOT_LISTEN and PICACHE_DOH_LISTEN are
  off".
- **Downgrade:** 0.13 works without the pre-upgrade copy, with these
  effects: it sets the newer `logs.db` aside as `logs.db.broken-<ts>` (the
  query log, statistics and warning history start fresh; after upgrading
  again the file can be moved back by hand while PiCache is stopped); plain
  DNS is on and DoT and DoH are gone; `clientid:` identifiers are skipped (a
  client with only ClientIDs never matches) and 0.13 refuses to save such a
  client until they are removed; `clientid:` entries of
  `dns.blockedClients` and upstream entries with `quic://`, `h3://` or
  `sdns://` make the stored settings invalid for 0.13 (it opens them with a
  warning and refuses every settings save until they are removed) and 0.13
  skips those entries: a default set, forwarder or group set with only such
  upstreams has none (SERVFAIL; group sets fail closed). Remove the new
  entries before going back, or restore the copy
  `<data>/backups/picache-<old version>-<timestamp>.db` made at the first
  start. Backups made by 0.14 are accepted by 0.13 with the same effects.
  A save by 0.13 drops `dns.plainDns` and `dns.encrypted`; upgrading again
  restores their defaults (DoT and DoH off, plain DNS on).

### Added

- **DNS over TLS for clients** (`dns.encrypted.dot`, `PICACHE_DOT_LISTEN`,
  default `:853`): RFC 7858 with the certificate of the web UI, behind the
  DNS access list at accept (before TLS), 32 connections per client and
  1024 per listener, queries of a connection answered one after another,
  EDNS padding (RFC 8467).
- **DNS over HTTPS for clients** (`dns.encrypted.doh`): RFC 8484 GET and
  POST at `/dns-query` on the HTTPS web listeners, on dedicated listeners
  (`PICACHE_DOH_LISTEN`, off by default; nothing but `/dns-query`) and
  behind a trusted reverse proxy; 64 requests in flight per client,
  cross-site browser requests refused, `Cache-Control` from the answer's
  TTLs.
- **Server name** (`dns.encrypted.serverName`): the name devices use for
  DoT and DoH, checked against the local domain and the search domains;
  PiCache answers it (and `<ClientID>.<serverName>`) with its own
  addresses. The local CA includes it and, while DoT is on,
  `*.<serverName>`; a name that equals or is a parent of the local domain,
  a search domain or an allowed web host is left out of a new local CA.
- **ClientIDs:** the first label of the DoT server name
  (`<id>.<serverName>`) or the DoH path (`/dns-query/<id>`) names a device.
  Clients take `clientid:<id>` identifiers (`clients.Client.identifiers`)
  and `dns.blockedClients` takes `clientid:<id>` entries. A ClientID only
  identifies devices that PiCache does not already identify by address or
  MAC; it never authenticates and never unblocks. The query log and its
  export show and filter the ClientID (`dnsClientId`), the lookup can test
  one, `GET /clients/dns-client-ids` lists the ClientIDs seen since the
  start and `GET /clients/known` shows them (`dnsClientId`).
- **Discovery of Designated Resolvers** (RFC 9462): `_dns.resolver.arpa`
  SVCB answers name the server name with DoT and DoH (only for its own
  addresses covered by the certificate, verified discovery), so Windows 11,
  Android and Apple devices can upgrade to encrypted DNS by themselves.
- **Plain DNS switch** (`dns.plainDns`): off answers other devices REFUSED
  (EDE 18 "Prohibited", reason `plain-dns-off`) over UDP and TCP while DoT
  or DoH is serving; this machine, the server name, its ClientID names and
  `_dns.resolver.arpa` stay answered. It fails open: while nothing
  encrypted is serving, plain DNS serves everyone, an error is logged and
  the health check fails.
- **Configuration profiles for Apple devices**
  (`GET /dns/profile.mobileconfig`, over HTTPS or on the host itself):
  DoH or DoT, an optional ClientID, the home Wi-Fi names the profile
  applies on (the device uses its normal DNS elsewhere; without names:
  everywhere) and optionally PiCache's addresses (`ServerAddresses`); links
  for a QR code (`POST /dns/profile-links`,
  `GET /dns/profile-links/{token}`), valid and reusable for 15 minutes,
  kept in memory.
- **Status of encrypted DNS** (`GET /dns/encrypted`): what is enabled and
  serving, the listeners, the URLs and host names for devices, the
  certificate (usable, covering the server name and the wildcard), the
  plain DNS switch and DDR. Health check `encrypted-dns`; the listeners
  check and `/system/info` know the roles `dot` and `doh`.
- **More upstream protocols:** DNS over QUIC (`quic://host[:port]`, RFC
  9250), DNS over HTTPS over HTTP/3 (`h3://host[:port]/path`) and DNS
  stamps (`sdns://…`) of DNSCrypt, DoH, DoT and DoQ resolvers, including
  DNSCrypt v2 (XSalsa20-Poly1305 and XChaCha20-Poly1305) and the stamps'
  certificate hashes as pins in addition to the normal verification.
  Everywhere upstreams are accepted (default and fallback upstreams,
  forwarders, group upstreams). An upstream that names PiCache itself is
  refused.

### Changed

- **Dependencies:** `github.com/quic-go/quic-go` (with
  `github.com/quic-go/qpack`) for the DoQ and HTTP/3 upstreams, only as a
  client: PiCache never listens on QUIC.
- The certificate of the web UI (certificate files, an upload, the local
  CA or self-signed) serves every TLS listener: the web UI, DoT and DoH;
  `web.tlsMinVersion` applies to all of them.
- A restore whose settings switch plain DNS off while no listener of this
  host serves their encrypted protocols warns (`dnsWarning`).
- The support bundle keeps the switches of encrypted DNS, scrubs the server
  name like the other host names and reduces DNS stamps to
  `sdns:<protocol>:<host>`.

## [0.13.0] - 2026-09-26

### Upgrade notes

- **Databases:** `picache.db` gets the rule modifiers, the list format and
  automatic name, the IP rules (filter migration 3), the record scope and
  other-family columns with their group links (dns migration 3) and the
  upstreams of client groups (clients migration 4). The migrations only add
  columns and tables: every rule, list and record keeps its groups, the list
  IDs stay, every existing record keeps answering everyone and every group
  keeps the default upstreams. `logs.db` has no migration.
- **Behaviour changes:** lines of subscribed lists with `$dnstype` (on
  exact and subtree lines) or `$denyallow` (on subtree block lines) were
  skipped as unsupported and now apply (the list's counts change after its
  next update or start).
  `dns.localizeRecords` is `first` by default: a local record with several
  addresses now answers the addresses in the client's network first
  (nothing is removed; set it to `off` for the stored order). A new query
  status `blocked-ip` (class blocked) appears once lists of answer
  addresses or IP rules are used.
- **API:** every new member of `RuleInput`, `ListInput`, `RecordInput` and
  `GroupInput` may be left out: a body written for 0.12 keeps the stored
  values (a stored member that no longer fits is reset). The dry runs of
  the imports are locked by `PICACHE_CONFIG_LOCKED` like the imports.
- **Downgrade:** a version before 0.13.0 refuses the migrated `picache.db`
  (clients schema 4, filter schema 3, dns schema 3) and does not start: go
  back with the copy `<data>/backups/picache-<old version>-<timestamp>.db`
  made at the first start (the update helper's rollback does this itself;
  Docker users restore it before starting an older image). `logs.db` stays
  readable; 0.12 shows `blocked-ip` rows with the raw status. Backups made
  by 0.13 are refused by 0.12; 0.12 backups are accepted and migrated.

### Added

- **Rules per query type** (`qtypes`, `qtypesNegate`): a rule applies only
  to the listed types (or to all but them); a rule for A or AAAA also
  covers HTTPS and SVCB. Every filter check passes the query's type, so
  an allow rule for A lifts nothing for AAAA.
- **A reply per rule** (`reply`, `replyIpv4`, `replyIpv6`): NXDOMAIN,
  NODATA, REFUSED, the null address or custom addresses instead of the
  blocking mode; `self` (this server's address) also for
  `filter.blockingIpv4`/`blockingIpv6`. A blocked download service name
  never gets this server's address (the download cache would serve it
  anyway): that address family is answered empty.
- **Exceptions and inverted expressions:** `denyallow` excepts
  subdomains from a subtree or regex block rule; `invert` makes a regex
  block rule block every name its expression does not match (at most 32).
- **Import and export of rules** (`POST /filter/rules/import` with a
  preview, `GET /filter/rules/export`): the list syntax with `$dnstype`,
  `$denyallow`, `$reply`, `$dnsrewrite` (addresses and NXDOMAIN, REFUSED,
  NOERROR), `$invert` and the `;querytype=`, `;reply=` and `;invert`
  suffixes of regular expressions; all or nothing, errors by line.
- **Batch changes** (`POST …/batch`): delete, enable or disable many rules,
  IP rules, lists, records, forwarders, groups (and delete clients) at
  once, all or nothing, audited once. Enabling lists beyond the entry
  budget asks for confirmation (`force`).
- **"Only for this device"** (`POST /filter/rules/device`, the query log):
  a rule for one device, through a client and a group of its own that
  never joins an existing group of the same name.
- **Blocking by answer address:** lists of the format `ips` (addresses and
  networks of malicious servers) and IP rules (`/filter/ip-rules`) block
  answers that contain a listed address (status `blocked-ip`); the IP
  guard ignores list entries for broad, private and special networks and
  for networks inside or around the IPv6 prefixes that carry IPv4
  addresses (`ipBlocksIgnored`, health warning); lists judge NAT64 and
  DNS64 addresses by the IPv4 address they carry.
- **List titles:** a list added without a name takes the title from its
  header after the first download (cleaned, at most 100 characters).
- **Search** (`GET /filter/search`): finds a text in your rules, IP rules
  and the downloaded lists (bounded, never downloads).
- **Local DNS:** record types SRV, MX, PTR, HTTPS and SVCB (with a
  structured `data` form); records per group (split horizon, `scope`,
  `groupIds`: a record scoped to deleted groups answers nobody, never
  everyone); `otherFamily: forward` asks the upstreams for the other
  address family; `dns.localRecordsEnabled` switches all records off;
  `dns.localizeRecords` answers the client's local addresses first or only;
  import from a hosts file (`POST /dns/records/import`, a pasted blocklist
  is recognised).
- **This server's addresses** (`dns.serverNameAddresses`) for macvlan and
  NAT set-ups: answered for PiCache's names and their PTR.
- **Upstreams per group:** a group's own resolver list or a family-safe
  preset (Cloudflare for Families, OpenDNS FamilyShield, CleanBrowsing
  Family Filter; `GET /groups/upstream-presets`,
  `PUT /groups/{id}/upstreams`). It stays on while blocking is paused,
  fails closed (SERVFAIL, no fallback, never the bootstrap servers) and
  shows in `GET /dns/upstreams` (`groups`) and the health check.
- **Explain** takes a query type and shows why an entry was skipped
  (`qtype`, `denyallow`).

### Changed

- The download cache closes the connection of a request for a host that
  is not a download service, and of every request while it is switched
  off (403 with `Connection: close`), so browsers sent to this server's
  address by a blocking reply do not use up the per-client connection
  limit.
- `filter.Stats` counts modified list entries and address entries
  (`modifiedEntries`, `modifiedDropped`, `ipEntries`, `ipRules`,
  `ipGuardLists`); `entries` includes them.

## [0.12.0] - 2026-09-26

### Upgrade notes

- **Databases:** `logs.db` gets the upstream's answer column, the warning
  history and daily top tables (logs migration 4, no table rewrite); the
  history of the last year appears in the daily tables within about 30
  minutes after the first start. `picache.db` gets `ignore_stats` for clients
  (clients migration 3): a client that was excluded from the logs stays
  excluded from both the raw data and the statistics.
- **Settings:** the new log settings start with their defaults (statistics
  on, domains not hidden, no ignored domains, every query type counted,
  written every 5 s; health thresholds 5 % memory, load 2 per CPU, 80 °C).
  Every existing combination of the query log and anonymisation keeps its
  behaviour; the privacy level shows `full` or `custom`.
- **API:** `logs.privacyLevel` is derived from the four privacy switches;
  a value sent in `PUT`/`PATCH /settings` is ignored. `ClientInput` without
  `ignoreStats` (or `null`) takes the value of `ignoreLogs`, so older API
  clients keep the meaning of the single flag. `GET /stats/top?kind=upstreams`
  carries the average duration in the new `avgDurationUs` (and still in
  `bytes`). Ranges longer than 7 days read daily top tables: their `topFrom`
  is the start of the UTC day.
- **Downgrade:** a version before 0.12.0 refuses the migrated `picache.db`
  (clients schema 3) and does not start: go back with the copy
  `<data>/backups/picache-<old version>-<timestamp>.db` made at the first
  start (the update helper's rollback does this itself). 0.11 sets the newer
  `logs.db` aside as `logs.db.broken-<timestamp>` (query log, statistics and
  warning history start fresh); after upgrading again, stop PiCache and move
  it back by hand. Backups made by 0.12 are refused by 0.11.

### Added

- **Privacy levels** (**System → Logs & privacy**, a new page that also holds
  the retention settings): *Full*, *Hide domains*, *Anonymous*, *Off* and
  *Custom* over four switches: the query log, anonymised client addresses,
  `logs.hideDomains` (domain names replaced by `hidden`, answers removed, no
  top lists of domains) and `logs.statsEnabled` (the DNS statistics). Per
  client, `ignoreStats` excludes the statistics and `ignoreLogs` the raw
  data (query log, cache requests, SNI events, download sessions, seen data).
  `logs.ignoredDomains` are answered and filtered but never logged or
  counted; `logs.statsOnlyAddressQueries` counts only A, AAAA and HTTPS
  queries in the statistics.
- **Clearing:** `DELETE /logs/queries` and `DELETE /stats` (admins,
  destructive, audited) clear the query log or the statistics through the
  log writer, so nothing reappears.
- **Dashboard:** top allowed domains, upstreams with their share and average
  response time, the average processing time, an estimate of the distinct
  domains (HyperLogLog, about 2.3 %), and a chart of the query types
  (`GET /stats/qtypes`, at most 32 types plus `OTHER`).
- **Long ranges:** 90 days, 180 days, a year and custom ranges read daily top
  tables (built after each day and backfilled for the existing history).
- **Client activity:** `GET /stats/clients/{key}/series`: allowed and
  blocked queries and download-cache bytes per step for an address or a
  device (client or MAC).
- **Query log:** filters by response code (`rcode`) and DNSSEC (`dnssec`),
  the upstream's answer when it differs from the final one (CNAME and
  upstream blocks, rebinding protection, bogus NXDOMAIN, DNS64, removed
  `ipv6hint`), "Known tracker" in the explain result for lists of the
  category privacy that block the name (`filter.Match.category`), and an
  export as NDJSON or CSV (`GET /logs/queries/export`, one at a time, at
  most 1 000 000 rows or 15 minutes, spreadsheet formulas neutralised).
- **CLI:** `picache logs tail` follows the query log, `picache logs export`
  saves it (API token from `PICACHE_TOKEN` or `--token-file`, never from
  `picache.env`; sent only to loopback or verified HTTPS URLs).
  `picache db check` checks `picache.db`; `picache db salvage --out <file>`
  copies every readable row of a damaged one into a new file.
- **Application log** (**System → Application log**, admins): the last 2000
  log records with level and component filters and a live stream, secrets
  redacted (also in the journal), client addresses and domains masked per
  the privacy switches; a temporary debug level for all components or one
  (`PUT`/`DELETE /system/log/level`).
- **Host resources** (`GET /system/host`, **Health & about**): model, CPUs,
  load, uptime, memory, the cgroup's memory, temperatures and the data and
  cache disks; the health check `host` warns about low memory, high load and
  heat (thresholds in the new settings section `health`).
- **Warning history** (`GET /system/events`, **Health & about**): the
  notification events are recorded whether or not channels exist, merged
  while unacknowledged and acknowledged by admins; the header shows the
  unacknowledged warnings (`/system/overview` `events`).
- **Support bundle** (`POST /system/support-bundle`, **Health & about**): a
  zip of version, settings, health, listeners, network check, DHCP state,
  database sizes, host resources and the application log with names,
  addresses and secrets replaced; client names only when ticked.
- **Database sizes** (`GET /system/databases`), DNS cache counters
  (insertions, evictions, expired, entries by type) and new metrics
  (`picache_dns_cache_*`, `picache_dhcp_*`).
- **Fewer SD-card writes:** `logs.flushSeconds` (5 to 300 s) sets how often
  the query log and the counters are written.

## [0.11.0] - 2026-09-26

### Upgrade notes

- **Web access stays open after the upgrade.** New installations allow the
  web UI only from this machine, the private networks, the networks the
  machine is connected to and the DNS allowed networks. An upgraded
  installation keeps it open to every address (settings migration 5 sets
  `web.restrictToNetworks` to `false`); **System → Users & security → Web
  access** recommends switching the restriction on. Monitors and scrapers of
  `/healthz` or `/metrics` outside these networks then need an entry in the
  allowed networks. If you lock yourself out, run `sudo picache web-access
  --reset` on the host.
- **Every existing account is an admin** (auth migration 2 adds roles); new
  accounts can be viewers. A restore keeps the roles of the running
  instance.
- **HTTPS certificate:** the existing self-signed certificate stays until 30
  days before it expires; then (or at once with *Create local CA now* under
  **System → HTTPS certificate**) PiCache switches to a certificate of its
  own local CA, which your devices can trust once. The HTTPS listener no
  longer stays down when a certificate cannot be loaded: PiCache serves a
  fallback certificate and the health check `tls` fails.
- **Downgrade:** a version before 0.11.0 refuses the migrated `picache.db`
  (auth schema 2, settings schema 5) and does not start. Going back needs the
  copy `<data>/backups/picache-<old version>-<timestamp>.db` that 0.11.0
  makes at its first start before any migration (the update helper's
  rollback restores it itself; Docker users restore it before starting an
  older image).

### Added

- **Web access control:** "Allow the web UI only from these networks"
  (`web.restrictToNetworks`, `web.allowedNetworks`): connections from
  other addresses are closed at accept and every request is checked again;
  refusals are counted and logged. Changes that would lock out your own
  address are refused. `picache web-access --reset` opens the web UI again
  from the host (applied within a minute, at once on SIGHUP, or at the next
  start).
- **Trusted reverse proxies** (`web.trustedProxies`): their
  `X-Forwarded-For` and `X-Forwarded-Proto` are read (never `Forwarded`
  or `X-Real-IP`), so sessions, the audit log, sign-in throttling and the
  web access see the real client, and a TLS-terminating proxy gets
  `Secure` cookies without `PICACHE_WEB_SECURE_COOKIES`. DEPLOYMENT.md has
  Caddy, nginx and Traefik examples.
- **Accounts with roles:** up to 32 accounts, admins and viewers (read-only;
  they manage only their own password, two-factor authentication, sessions
  and read tokens), managed under **System → Users & security**
  (`/system/users`); at least one admin always remains. `picache users`
  lists the accounts; `picache reset-password --admin <user>` makes an
  account an admin. API tokens list their owner; viewers see and delete
  only their own and create read tokens only; 20 tokens per account.
- **HTTPS certificates:** a local CA with critical name constraints (only
  PiCache's own names and addresses) issues the web certificate and renews
  it; the CA certificate can be downloaded (`/system/tls/ca.crt`) with a
  trust guide per platform. Upload your own certificate and key
  (`PUT /system/tls`, over HTTPS only), or use certificate files that
  PiCache now reloads within a minute when they change (at once on SIGHUP),
  e.g. from Let's Encrypt with a DNS-01 deploy hook (guide in
  DEPLOYMENT.md). New health check `tls` (fallback in use, expired or
  expiring certificate, expiring local CA, names the CA does not cover).
- **Minimum TLS version** `web.tlsMinVersion` (`1.2` or `1.3`), applied to
  the next handshake.
- **Configuration lock** `PICACHE_CONFIG_LOCKED`: browser sessions cannot
  change the configuration (`config_locked`), admin API tokens can (for
  infrastructure as code; not an access control). `PICACHE_DESTRUCTIVE_API=false`
  refuses restores, resets, purges and other bulk deletions through the API.
  `GET /auth/status` reports both.
- `GET /system/info` reports `webRefused`, `clientAddress` and
  `peerAddress`; a restore whose settings would lock you out answers with
  `webAccessWarning`.

### Changed

- New permission **U** (own account): the password, two-factor
  authentication, sessions and API token routes work for every browser
  session (viewers included) and never for API tokens.
- An admin API token acts with admin rights only while its owner is an
  admin; a demotion deletes the account's admin tokens and ends its
  sessions.
- `picache reset-password` prints the account's role; a username is
  validated only when an account is created, so any stored name can be
  reset, and provisioning never stops the start because of a stored name.
- SIGHUP no longer ends PiCache; it checks the web access reset marker and
  the certificate files at once.
- The HTTPS redirect and HSTS follow the effective scheme (a trusted proxy's
  `X-Forwarded-Proto: https`).
- A sign-in, API token, password or two-factor change whose password check
  overlapped a password reset, a demotion or `picache reset-password` is
  refused instead of surviving it; a sign-in no longer overwrites a password
  that was reset meanwhile when it upgrades the stored hash.
- `picache reset-password` run with a new version before its first start
  makes the pre-upgrade copy of `picache.db` first (it migrates the accounts
  table), so going back still works.
- `POST /dns/blocking` refuses a `pauseSeconds` above 604800 before using
  it (a huge value could turn into a permanent disable).
- Only `picache serve` reads `PICACHE_ADMIN_PASSWORD_FILE`: the maintenance
  commands (`web-access --reset`, `users`, `reset-password`, `setup-token`,
  `update`) ignore it, so a root-only secret file left in a Docker setup no
  longer breaks the recovery commands.


## [0.10.0] - 2026-09-26

### Upgrade notes

- **Migrations**: filter 2 adds a category and a catalogue key to every
  list (`category`, `catalogKey`; existing lists get the category of the
  catalogue entry with the same URL, else `other`, `allow` for
  allowlists, and `abused-tlds` for an own blocklist whose downloaded copy
  blocks mostly whole top-level domains); parental 2 adds the pause of a
  group's filtering (`pause_until`). Nothing else is rewritten; `logs.db`
  is unchanged.
- **Lists that block whole top-level domains need the category
  `abused-tlds`**: the parser now ignores entries such as `||zip^` or
  `*.co.uk^` in every other list (see Changed). An own list of abused TLDs
  that is not recognised at the upgrade keeps working only after you give
  it that category (Filtering → Blocklists); such a list shows how many
  entries it ignores, and the health check `blocklists` names it.
- **The HaGeZi DoH/VPN/TOR/Proxy Bypass list is now a protection list**
  (category `doh-vpn-bypass`): it applies like parental controls, also
  while blocking is paused or disabled, and the allow override of a group
  does not lift it. Set its category to `security` (Filtering →
  Blocklists) to get the old behaviour; the parental switch "Bypass
  services" then shows off for its groups (the list pauses with blocking),
  and switching it on gives the list its category back, for all its groups.
- **Special domains no longer follow the blocking switch**: the Mozilla
  canary (`use-application-dns.net`) and iCloud Private Relay are answered
  by their own settings also while blocking is paused or disabled (only an
  allow rule or allowlist of the client exempts them). A browser that saw
  the canary unblocked during a pause switched to encrypted DNS and kept
  it, which bypassed parental controls.
- **Downgrade**: a version before 0.10.0 refuses the migrated `picache.db`
  (newer schema) and does not start. Going back needs the copy
  `<data>/backups/picache-<old version>-<timestamp>.db` that 0.10.0 makes at
  its first start (the update helper's rollback restores it itself; Docker
  users restore it before starting an older image). `logs.db` keeps its
  schema: 0.9.0 ignores the new statistics rows (kind `purpose`) and shows
  logged safe-search queries with their raw status `safesearch`.

### Added

- **Safe search per group** (parental controls): Google, YouTube
  restricted mode (moderate or strict), Bing, DuckDuckGo, Ecosia, Yandex
  and Pixabay are answered with a CNAME to the engine's restricted host,
  whose own answer is resolved like any other (conditional forwarders,
  DNS64, rebind protection). HTTPS/SVCB/ANY queries of these names get no
  data, other types (MX, TXT, DS) are answered normally. New query status
  `safesearch` (an allowed status). It stays on while blocking is paused
  or disabled, during a group's allow override and against allow rules;
  a block of the name for the client (a deny rule, a list) wins.
- **Category switches** (parental controls): adult content, gambling,
  dating, piracy and DNS/VPN bypass per group, each through one
  catalogue list that PiCache downloads (OISD NSFW, HaGeZi Gambling
  medium, ShadowWhisperer Dating, HaGeZi Anti-Piracy, HaGeZi
  DoH/VPN/TOR/Proxy Bypass). The PUT body of `/parental/groups/{id}`
  takes optional `safeSearch` and `categories`; a body without them
  changes neither.
- **Protection lists**: every enabled list of the categories `adult`,
  `gambling`, `dating`, `piracy` and `doh-vpn-bypass` is enforced like
  parental controls, also while blocking is paused or disabled; an
  allowlist does not lift it, a user allow rule for the client does.
- **List catalogue** of 68 lists in categories (general, security,
  privacy, adult, gambling, dating, piracy, social, DoH/VPN bypass, abused
  TLDs, URL shorteners, stalkerware, regional, allowlists), each with its
  maintainer, license, homepage, entry count and an English and German
  description; every URL was checked for this release. Lists have a
  category (`filter.List.category`, also for your own lists).
- **Pause per group**: `PUT/DELETE /parental/groups/{id}/pause` pauses the
  lists and rules of one group for up to 7 days (audited as
  `parental.pause` and `parental.pause_clear`); parental controls, safe
  search and protection lists stay on; the client's other groups still
  apply. The global pause offers "until 06:00" and custom durations;
  `/dns/blocking` reports the host's time zone (`timeZone`,
  `utcOffsetMinutes`).
- **Blocked services**: 144 services (was 27) in 13 categories, now also
  dating, gambling, shopping, VPN and proxy apps, app stores, file
  hosting and news; up to 256 services per group or schedule.
- **Statistics by purpose** (`GET /stats/purposes`): blocked and
  safe-search queries by list category, rule, service, schedule,
  upstream block, rebinding, special domain and safe search.
- A health warning when the blocklists hold more than 4 000 000 entries
  (the memory of a 1 GB host).

### Changed

- The list parser counts a subtree, wildcard or pattern block of a single
  label or of an ICANN public suffix (`||com^`, `*.co.uk`, `||*.com^`,
  `.com^`, `/\.xyz$/`) as invalid, so one broken or hostile list cannot
  block a whole top-level domain; lists of the category `abused-tlds` are
  exempt. `filter.List.tldBlocksIgnored` counts the ignored entries.
- At start, the counts of a list (`entries`, `invalid`, `unsupported`)
  follow its cached copy as this version parses it.
- An empty `kind` of a new list with a catalogue URL takes the entry's kind;
  a kind that contradicts the catalogue entry is refused (field `kind`).

## [0.9.0] - 2026-09-26

### Upgrade notes

- **Rebinding protection is on.** Answers from the upstreams that point a
  public name at a private, loopback or link-local address are now blocked
  and appear as **Rebinding blocked** (`blocked-rebind`) in the query log.
  If a service you use answers with LAN addresses on purpose, allow its
  domain (**Allow rebinding for `<name>`** in the query panel, or
  `dns.rebindAllow`; `plex.direct` is allowed by default). The names in
  `web.allowedHosts` are allowed automatically. A local resolver used as
  default upstream (a router, unbound) needs its domains allowed or, better,
  a conditional forwarder for them. DNSBL zones queried through PiCache
  (e.g. by a mail server) must be allowed too.
- **Bare names stay local.** A query for a name without a dot (`nas`) of
  type A, AAAA, HTTPS, SVCB or ANY is answered as `nas.<local domain>` from
  local records, DHCP names, conditional forwarders or the router, and is no
  longer sent to the upstreams; `wpad` and `isatap` are answered only from
  local records (`dns.domainNeeded`, on).
- **Quad9's blocks are visible**: they appear as **Blocked by upstream**
  (`blocked-upstream`) and count as blocked in the statistics.
- **Default upstreams**: an installation on the old default list (Quad9 and
  Cloudflare) now uses Quad9 only, with Cloudflare's malware-filtering
  resolver as fallback when Quad9 does not answer. An installation with its
  own upstream list keeps it and gets no fallback (turn it on in **DNS
  settings → Upstream DNS servers → Fallback DNS**). A network that blocks both Quad9 and Cloudflare
  needs its own upstreams.
- **Downgrade**: a version before 0.9.0 refuses the migrated `picache.db`
  (newer schema) and does not start. Going back needs the copy
  `<data>/backups/picache-<old version>-<timestamp>.db` that 0.9.0 makes at
  its first start: the rollback of the update helper (systemd) restores it
  itself, Docker users restore it before starting an older image. An older
  version cannot open the newer `logs.db` and sets it aside, so the query
  log and the statistics start fresh after a downgrade.

### Changed

- **Default upstreams: Quad9 only** (`https://dns.quad9.net/dns-query`,
  which filters malware), with **Cloudflare's malware-filtering resolver
  as fallback** (`https://security.cloudflare-dns.com/dns-query`, another
  operator). Load-balancing Quad9 with an unfiltered resolver made its
  malware blocking random. Settings schema v4 converts stored settings
  (see the upgrade notes).
- The DNS rate limit can count a whole public network as one client
  (`dns.rateLimitIpv4Prefix`, `dns.rateLimitIpv6Prefix`; LAN sources are
  still limited per address; the defaults keep today's keys), and trusted
  EDNS forwarders are exempt from it. Login throttling and the other limits
  are unchanged.

### Added

- **DNS rebinding protection** (`dns.rebindProtection`, on;
  `dns.rebindAllow`): answers of the default upstreams, the fallbacks and
  `default` forwarders that point names at private, loopback or link-local
  addresses (also as IPv4-mapped, IPv4-compatible, 6to4, NAT64 or DNS64
  addresses) are blocked; such addresses are removed from HTTPS/SVCB hints
  and the additional section. Also while blocking is paused.
- **Detection of answers blocked by the upstream**: EDE 15–17, 0.0.0.0/`::`,
  Cisco Umbrella block pages and Quad9's NXDOMAIN without RA become the
  blocking reply with status `blocked-upstream` and the reason
  `<host>: <kind>` (never the path of a DoH URL; clients get only
  `blocked by upstream (<kind>)`, since a host name can carry a profile
  ID); they are cached for
  `dns.upstreamBlockedTtl` (300 s). The upstream's EDE (any code) is shown
  in the query log (`upstreamEde`).
- **Fallback DNS** (`dns.fallbackUpstreams`, at most 4): asked only when no
  default upstream replied at all (never after an error reply), within the
  10 s a query may take; `GET /dns/upstreams` shows their statistics
  (`fallbacks`, `fallbackLastUsed`) and the health check warns while a
  fallback answers.
- **Keep bare names local** (`dns.domainNeeded`, on) and extra **private
  reverse networks** (`dns.privateReverseNetworks`) whose PTR queries are
  answered locally like the RFC 6303 zones.
- **Blocked clients** (`dns.blockedClients`: addresses, networks or MAC
  addresses; at most 256): their DNS queries get no answer (DNS only; the
  download cache is unaffected). **Block device** in the query panel and
  the Seen recently list (`POST /dns/blocked-clients`, `DELETE
  /dns/blocked-clients?entry=`; `GET /clients/known` rows have
  `blockedBy`). PiCache refuses entries that would block itself, the
  router, the container network's gateway or a trusted forwarder (their
  MAC addresses included), and never drops them at run time.
- **Dropped domains** (`dns.droppedDomains`, optionally per query type): no
  answer, not logged. **Bogus NXDOMAIN** (`dns.bogusNxdomain`): answers
  with listed addresses become NXDOMAIN.
- **Conditional forwarders**: several domains per forwarder (`domains`), the
  target `default` (an exception back to the default upstreams, e.g.
  `public.corp.example` inside `corp.example`), the domain `(unqualified)`
  for bare names, and an import of dnsmasq-style lines
  (`[/corp.example/]192.168.1.1`, `#` = default, `[//]` = bare names) with
  a preview (`POST /dns/forwarders/import`).
- Plain DNS upstreams and forwarder targets may be given by a public host
  name (resolved through the bootstrap servers; only public addresses are
  dialled). **Prefer IPv6** for DoT, DoH and named upstreams
  (`dns.bootstrapPreferIpv6`).
- Upstream mode **`fastest_addr`**: the upstreams are asked in parallel and
  the address of an answer that connects fastest (TCP 443, else 80;
  bounded, public addresses only) comes first. Off by default.
- **EDNS client subnet** to the default upstreams (`dns.ecs`: off, the
  client's /24 or /56 for public addresses, or a fixed public network), and
  the subnet a client sent is shown in the query log (`ecs`).
- **Clients behind a trusted forwarder** (`dns.ednsClientTrusted`): their
  address (ECS) and MAC (option 65001) identify them, so groups, parental
  controls and the query log see the real devices. The forwarder must strip
  its clients' own options.
- `dnsserver.Stats` has `blockedClients` and `dropped`; `/metrics` has
  `picache_dns_blocked_clients_total` and `picache_dns_dropped_total`.
  `POST /dns/lookup` traces every new step and reports `dropped`.

## [0.8.0] - 2026-09-26

### Changed

- **The DHCP server needs no installation option any more.** It is
  available on every Linux installation and switched on in the web UI
  (**DNS → DHCP**) when needed; `install.sh --with-dhcp` and
  `PICACHE_DHCP=on` are no longer needed. **Nothing is held while DHCP is
  off**: PiCache opens UDP ports 67 and 547 only while the DHCP server is
  switched on (a search for other DHCP servers opens port 67 for its 5
  seconds) and never binds 546; the passive detection of other DHCP
  servers (a device asking another server) therefore runs only while it is
  on. PiCache remembers what to open at the next start in two empty files
  in the data directory (`dhcp.sockets`, `dhcp.ra`).
- `PICACHE_DHCP` has three states: unset (the default: the DHCP server can
  be switched on in the web UI), `off` (it cannot; for hosts that run
  another DHCP server) and the old `on` (still accepted: everything opens at
  start and PiCache closes what is not needed). **`install.sh
  --without-dhcp` now writes `PICACHE_DHCP=off`** instead of removing the
  DHCP support; `--with-dhcp` removes that opt-out again. Every installer
  run removes an old `PICACHE_DHCP=on` (creating the two DHCP markers in
  its place, so the first start of 0.8.0 still opens the DHCP ports and
  the raw socket for router advertisements), the drop-in
  `/etc/systemd/system/picache.service.d/60-dhcp.conf` and
  `/etc/picache/dhcp.enabled`, and rewrites any off spelling as
  `PICACHE_DHCP=off`; a value it does not know is reported (PiCache refuses
  to start with it). It also replaces the DHCP comment of a 0.7.0
  `picache.env`, which still described the old install option. A leftover `60-dhcp.conf` on an installation updated
  only from the web UI is harmless (it holds the same settings as the new
  unit) and goes with the next installer run.
- **Docker needs one restart after switching DHCP on:** the container opens
  its DHCP ports only at start (the switch to 65532 clears every
  capability), and the page offers the restart. The compose file now has
  `NET_RAW` in `cap_add` (used only at start for the router
  advertisements); an existing compose file keeps working for DHCPv4 and
  DHCPv6, and the page says what to add for router advertisements.
- **Router advertisements need one restart after switching them on**: the
  raw socket can only be opened at start. The systemd unit now grants
  `CAP_NET_RAW` for it (with `capset` allowed) on every installation;
  PiCache drops it on every thread right after every start and now
  **refuses to run** if that fails (before, it only closed the socket and
  warned). If the drop cannot be verified, the raw socket is closed and the
  health check `dhcp` fails.
- **Updates also install the unit files** of the release (only from the
  signed `picache-deploy.tar.gz`, only PiCache's own units that are
  installed, never drop-ins; the old ones kept as `<unit>.prev`), where the
  update helper may write them; if that fails, only the program is updated
  and the message says so. **Existing installations run the one-line
  installer once** to get the new units (`CAP_NET_RAW` for router
  advertisements, the resource weights and an update helper that may
  replace unit files); from then on updates from the web UI and the CLI keep
  them current (`sudo picache update` does so from its first update after
  0.8.0 anyway). The update to this version itself is installed without its
  units. `install.sh --uninstall` removes the `.prev` copies.
- `picache.service` has `CPUWeight=200` and `IOWeight=200`: under CPU or
  disk contention PiCache gets twice the share of a service with the default
  weight (a mild bias; no effect where the cgroup controller is not
  delegated or the I/O scheduler ignores weights).
- Two devices with the same host name: the second now gets its generated
  name (below) instead of no DNS name.

### Added

- DHCP reservations: import and export (CSV with a guard against
  spreadsheet formulas, hosts format, and `mac ip [hostname]` lines; with a
  preview, all or nothing, optionally replacing all reservations), a lease
  time per device, and matching by client identifier (option 61) for
  devices that change their MAC address (as forgeable as the MAC: a
  convenience, not a security control). API: `GET /dhcp/static/export`,
  `POST /dhcp/static/import`; reservations have `clientId` and
  `leaseSeconds`.
- Generated DNS names for leases without a usable host name, such as
  `192-168-178-23.lan` (`dhcp.generateNames`, on by default; DNS answers
  only). Devices can no longer take such names, `wpad` or `localhost`.
- DHCP options NTP servers, interface MTU, WPAD URL (sent only when a
  device asks; changes of the WPAD URL are audited with the value) and
  extra search domains (`dhcp.options`).
- **Only reserved devices** (`dhcp.onlyReserved`): devices without a
  reservation get no answer from PiCache, so it can serve known devices next
  to another DHCP server.
- **End all leases** (`DELETE /dhcp/leases`) and **Reset DHCP**
  (`POST /dhcp/reset`: settings back to their defaults, every reservation
  and lease deleted; detections of other servers are kept).
- A log of the last 200 DHCP exchanges (`GET /dhcp/log`) and **rapid
  commit** (option 80; `dhcp.rapidCommit`, new and **off by default**, used
  only while no other DHCP server counts).
- PiCache looks for other routers and DHCPv6 servers that announce their own
  DNS server while it announces itself over IPv6 (a router solicitation and
  a relayed DHCPv6 information request, and every router advertisement seen
  while its own are on) and warns about them (page, health check `dhcp`,
  notification). The network check's `ipv6-dns` data carries the DNS
  servers the default router announces (`routerRdnss`).
- `GET /dhcp` has `reasonCode` (`opt-out`, `not-linux`, `bridge`, `socket`,
  `restart-required`), `markerError` and `deployment`; the router
  advertisements have a `reasonCode` too, and `ipv6.otherAnnouncers` (with
  `ownDns`, the announced DNS servers that are PiCache's own) and
  `ipv6.lastSearch`.

## [0.7.0] - 2026-09-25

### Added

- Optional DHCP server (**DNS → DHCP**), for routers that cannot hand out
  another DNS server: IPv4 addresses from a range on one chosen interface,
  with the router, PiCache as DNS server and the local domain; static
  leases; the devices' host names become DNS names (`laptop.lan` and the
  reverse name), shown in the query log and client lists. Off by default:
  it needs `install.sh --with-dhcp` (Docker: `PICACHE_DHCP` and host
  networking) and serves only when PiCache's own address is static and no
  other DHCP server answers (PiCache looks for one before it serves and
  every 10 minutes, and notices devices that ask another one). New API
  routes under `/dhcp`, the settings section `dhcp` and the health check
  `dhcp`.
- IPv6 DNS announcements of the DHCP server: router advertisements that
  carry only PiCache's ULA as DNS server and the domain (router lifetime 0,
  no prefixes: PiCache never becomes a router) and stateless DHCPv6. The
  raw socket needs `CAP_NET_RAW` at start (`install.sh --with-dhcp`, Docker
  `cap_add: [NET_RAW]`); PiCache drops it right afterwards on all threads
  and checks that it is gone.
- The network check says when PiCache hands out addresses itself (the
  router's IPv4 DNS steps then do not apply) and when it announces itself
  as IPv6 DNS server.
- `install.sh --with-dhcp` / `--without-dhcp` (also through
  `get-picache.sh`).

## [0.6.0] - 2026-09-25

### Added

- IPv6 parity: a client configured by its IPv4 address or a network also
  covers the device's IPv6 addresses on the same network, privacy addresses
  included (PiCache learns them from the device's MAC address in the
  neighbour table; for a new address it resolves the MAC before answering
  its first query, so parental controls cannot be bypassed over IPv6). An IPv6
  address without a name of its own shows the name of the device's IPv4
  address.
- Statistics per device: the overview's top clients and **Clients &
  groups** can show one row per device with all its addresses instead of one
  row per address. API: `GET /stats/clients` and `GET /stats/top` take
  `group=device`; client statistics carry `clientId`, `mac` and `addresses`;
  the query log and its live stream take repeated `client` values.
- **Allow every network this machine is connected to**
  (`dns.trustConnectedNetworks`, off by default): devices with a public IPv6
  address of the LAN may use PiCache, and a new prefix from the provider is
  followed within a minute. The network check offers it for refused
  devices of the local network and notes when PiCache's machine ignores
  IPv6 router advertisements.
- **Do not answer IPv6 addresses (AAAA)** (`dns.disableAAAA`) for networks
  whose IPv6 does not work, and **DNS64** (`dns.dns64`) for IPv6-only
  networks with a NAT64 gateway.
- The router resolver works over IPv6 when there is no IPv4 default gateway,
  and every address of the router is protected from forwarding loops and
  exempt from the rate limit.
- The default bootstrap servers include the IPv6 addresses of Quad9 and
  Cloudflare; an unchanged default list gets them with the update.

### Fixed

- Server-name answers no longer include deprecated or tentative IPv6
  addresses, prefer stable addresses over temporary ones and unique local
  addresses over global ones, and follow address changes within a minute.
- NODATA answers of the blocking modes `null` (other query types) and
  `custom_ip` (no blocking address of the queried family) carry the
  synthetic SOA like the other negative answers.
- The rate-limit help text: devices are limited per address; public IPv6
  addresses per /64. The access settings no longer say that the connected
  networks may always use PiCache (only private ones may).
- Client names are looked up through a router resolver with an IPv6 address
  too (the lookups were never sent).

## [0.5.0] - 2026-09-25

### Added

- Parental controls: **DNS → Parental controls** restricts the devices of a
  client group. Services from a built-in catalogue (YouTube, TikTok,
  Instagram, WhatsApp, Discord, Roblox, Fortnite, Steam, Netflix, ChatGPT and
  more) can be blocked always; up to 10 weekly schedules per group block all
  internet (a bedtime, also overnight) or selected services (homework time);
  "Block internet now" and "Lift restrictions" work for a set time. The
  page shows each group's state and weekly plan and tests a domain for a
  device. Parental controls apply even while blocking is paused, a user
  allow rule lets a name through, and blocked queries appear in the query
  log with the new statuses `blocked-schedule` and `blocked-service` and the
  group as reason. API: `/parental/services`, `/parental/groups`.
- Network check: **DNS → Network check** compares the devices in the
  network (the kernel's neighbour table) with PiCache's DNS queries and
  finds a router that forwards all queries or announces itself as IPv6 DNS
  server, missing IPv6 addresses of PiCache, sources refused by the access
  list, and devices that do not use PiCache (named as the router knows
  them), with the steps to fix it for a
  FRITZ!Box and other routers. An optional scan (admins) makes switched-on
  devices show up. The new health check `network` warns while most queries
  come from the router. API: `/network/check`, `/network/scan`.

## [0.4.0] - 2026-09-25

### Added

- Notifications: **System → Notifications** sends messages through ntfy,
  Gotify or a webhook (for example Home Assistant) when a health check fails
  or warns (and recovers), the cache storage goes offline (and back), an
  update is available, installed or fails, a scheduled backup fails or
  succeeds, or sign-ins are locked out. Per channel: minimum severity, event
  filter, test button; secrets are sealed with the master key and
  write-only; a delivery log shows the last attempts.
- Scheduled backups: daily or weekly at a set time, keeping the newest N
  files, to the data directory or to a storage target such as the NAS;
  missed runs are made up after a restart; run now, download and delete in
  **System → Backup & restore**. The time is that of the host, and the page
  shows its time zone (a Docker container uses UTC unless `TZ` is set).

## [0.3.1] - 2026-09-25

### Fixed

- NAS mounts through the root helper: a missing `mount.nfs` (package
  `nfs-common`) or `mount.cifs` (`cifs-utils`) is now reported with the
  package to install, instead of the kernel's misleading "Server address does
  not match proto= option" for NFS. The installer also says that NFS 4 does
  not need `rpcbind`.
- The storage page's hint for NFS permission errors names the settings of
  common NAS systems (TrueNAS Mapall User/Group, Synology Squash).

## [0.3.0] - 2026-09-25

### Added

- Storage speed test: **Cache → Storage → Test speed** measures write,
  read, cache-read (the path of a cache hit) and file-operation speed of a
  storage target, compares it with network speeds and keeps the last result
  per target.

### Documentation

- How to put the NAS on a separate storage network (no PiCache setting
  needed) and when that helps.

## [0.2.0] - 2026-09-25

Update from v0.1.0 on **System → Updates** or with `sudo picache update`
(Docker: `docker compose pull && docker compose up -d`). Settings, query
log and statistics are migrated automatically at the first start; only
API clients and scripts need the new names below.

### Changed

- **Breaking:** the download cache has new technical names. Update API
  clients and scripts that use the old ones.
  - Its API routes are now under `/api/v1/download-cache/...` (the
    sub-paths are unchanged).
  - Its settings section is now `downloadCache`, for example
    `downloadCache.cacheIpv4` and `PATCH /api/v1/settings/downloadCache`.
    Stored settings and restored backups are migrated automatically.
  - DNS queries answered with the cache address have the status
    `override`, also in the statistics series. The query log and the
    statistics are migrated automatically.
  - The related JSON fields are `downloadCacheEnabled`,
    `downloadCacheBypass` and `dnsDownloadCache`.
  - Its health check is `download_cache`, and new audit log entries use
    `download_cache.*` (existing entries keep their names).
  - Its Go package is `internal/dlcache`.
- Unchanged: the names that Steam and prefill tools rely on, that is the
  hostname Steam uses to discover a download cache, the heartbeat path that
  prefill tools probe and the response header they check.

## [0.1.0] - 2026-09-25

First release.

### Added

- Filtering DNS server: blocklists in hosts, domain, adblock (ABP) and regex
  formats with a built-in catalogue (HaGeZi Multi NORMAL by default), your
  own allow and block rules, groups for clients by IP, CIDR or MAC (clients
  get the lists and rules of their groups), five blocking modes, a timed
  pause, CNAME inspection, and blocking of the Firefox DoH canary and iCloud
  Private Relay.
- Local DNS records (A, AAAA, CNAME, TXT, wildcards, automatic PTR),
  conditional forwarding, and the router as resolver for local names and
  reverse lookups.
- Upstreams over DNS-over-HTTPS, DNS-over-TLS, UDP and TCP in load-balance,
  parallel or strict mode, with a response cache, serve-stale and a clock
  guard for devices without a real-time clock.
- Protection against open-resolver abuse: private networks only by default,
  per-client rate limits, `ANY` and CHAOS queries refused, private reverse
  zones never forwarded to public upstreams.
- Download cache: DNS answers that send downloads to the cache, from the
  cache-domains lists
  ([uklans/cache-domains](https://github.com/uklans/cache-domains)) plus
  custom services and hosts, an HTTP slice cache on port 80 (range requests,
  merged concurrent downloads, read-ahead), SNI pass-through on port 443,
  Steam's cache discovery, and the heartbeat path and response header that
  prefill tools use.
- Content grouping of cached downloads (Steam depots, Blizzard products,
  Epic, Riot, Xbox packages, Windows KBs, PlayStation titles, …) with your
  own labels, pinning, retention by inactivity and size, background verify
  and rebuild.
- Cache storage on local disks or SMB/NFS shares, with a mount guard,
  generated mount snippets and an optional root helper that mounts shares on
  request of the web UI.
- Web UI in English and German with overview, query log, live streams,
  statistics, filtering, clients and groups, local DNS, downloads, library,
  services, storage, settings, audit log, backup and restore, and health
  checks.
- One admin account created with a one-time setup token, optional TOTP
  two-factor authentication, API tokens with `read` or `admin` scope, and
  optional Prometheus metrics.
- REST API under `/api/v1` with Server-Sent Events for the live streams.
- Deployment: static binaries for linux/amd64, linux/arm64 and linux/arm
  (v7), a Debian 12/13 installer with hardened systemd units, a Proxmox LXC
  guide, and a distroless Docker image that drops root after binding its
  ports. The installer and the image put `LICENSE` and
  `THIRD_PARTY_NOTICES.md` into `/usr/share/doc/picache/`.
- CI: web UI type check and build, Go vet and tests on Linux, Windows and
  macOS, govulncheck, release builds, an installer smoke test and a
  multi-arch Docker build.
- Updates: **System → Updates** shows the installed version and the newest
  release with its notes. PiCache asks the GitHub API once a day whether a
  new release is out (can be turned off; pre-releases only on request) and
  never installs anything by itself. An admin installs an update with the
  password; a root helper (`picache-update.path` and `.service`, installed
  by `install.sh` unless `--without-updater`) downloads the release, checks
  its signature and checksum, replaces the binary, restarts PiCache and
  rolls back to the previous binary and database if the new version fails
  its health check. The web UI never offers a downgrade.
- `picache update`: `--check` (exit code 10 when an update is available),
  install the newest release or `--version vX.Y.Z` with the same checks and
  rollback, `--prerelease`, `--allow-downgrade`, and offline installs with
  `--from <dir>`.
- Signed releases: pushing a tag `vX.Y.Z` builds the static binaries,
  `picache-deploy.tar.gz` (installer, units, compose files, license texts)
  and `SHA256SUMS` with `make dist`, signs `SHA256SUMS` with the project's
  Ed25519 release key (`docs/release-key.pem`, also built into PiCache) and
  publishes a GitHub release with the matching section of this file as
  notes. Tags with a hyphen become pre-releases.
- One-line installer: `curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh`
  installs the newest release after checking its signature, upgrades an
  existing installation when run again, and removes PiCache with
  `--uninstall` (`--purge` also deletes the configuration, the data, the
  local cache and the picache user).
- Container images for linux/amd64, linux/arm64 and linux/arm/v7 at
  `ghcr.io/hustenreizjuengling/picache`, tagged `X.Y.Z`, `X.Y` and `latest`.
  Docker installations update with
  `docker compose pull && docker compose up -d`.

[Unreleased]: https://github.com/Hustenreizjuengling/PiCache/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.17.0...v1.0.0
[0.17.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.16.1...v0.17.0
[0.16.1]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.16.0...v0.16.1
[0.16.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Hustenreizjuengling/PiCache/releases/tag/v0.1.0
