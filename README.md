# PiCache

**PiCache is the DNS server and download cache for your home lab or LAN party,
in one binary.** It filters ads and trackers for the whole network, like
Pi-hole or AdGuard Home. It also works as a LanCache-compatible cache that
serves game and OS downloads (Steam, Epic, Battle.net, Riot, Xbox, Windows
Update, PlayStation, Nintendo, …) from local disk or a NAS after the first
download. One web UI shows who asked for what, what is cached and how much
bandwidth was saved.

It is written in Go with an embedded Svelte UI. No nginx, BIND, dnsmasq or cron
runs underneath: one process, one configuration database. It runs well on a
Raspberry Pi 4 or a small LXC container.

## Features

**DNS filtering**

- Blocklists in hosts, domain, AdGuard/ABP and regex formats (HaGeZi Multi
  NORMAL by default; OISD, StevenBlack, AdGuard DNS filter, 1Hosts and more
  in the built-in catalogue), updated automatically.
- Your own allow and block rules, including subdomain and regex rules, which
  always win over lists. The UI explains why a name was blocked.
- Groups with Pi-hole semantics: clients (by IP, CIDR or MAC) get the lists
  and rules of their groups.
- Blocking modes (null IP, NXDOMAIN, NODATA, REFUSED, custom IP), a timed
  pause, CNAME inspection, and blocking of the Firefox DoH canary and iCloud
  Private Relay.
- Local DNS records (A, AAAA, CNAME, TXT, wildcards, automatic PTR),
  conditional forwarding, and local names and reverse lookups from your
  router.
- Encrypted upstreams (DNS-over-HTTPS and DNS-over-TLS; plain UDP/TCP too)
  with load balancing, a response cache and serve-stale.
- Safe by default: not an open resolver (private networks only), rate limits,
  private reverse zones never leak upstream.

**Download cache (LanCache-compatible)**

- DNS overrides for the game and OS CDNs from
  [uklans/cache-domains](https://github.com/uklans/cache-domains), plus your
  own services and hosts. LanCache is off until you enable it.
- HTTP cache on port 80 that stores 1 MiB slices, handles range requests,
  merges concurrent downloads of the same content and reads ahead.
- HTTPS pass-through on port 443 (the connection is relayed by SNI, never
  decrypted), heartbeat and prefill-tool compatibility.
- "What was downloaded": content grouped into games and updates (Steam
  depots, Blizzard products, Epic, Riot, Xbox packages, Windows KBs,
  PlayStation titles, …) with your own labels.
- Retention by inactivity and size, pinning, background verify and rebuild.
- Cache storage on a local disk or on an SMB/NFS NAS, with a mount guard that
  never writes into an unmounted directory.

**Web UI and operations**

- English and German UI with dashboard, live query and download streams,
  query log, statistics and health checks with hints.
- Accounts with TOTP two-factor authentication, API tokens (`read`/`admin`),
  an audit log, backup and restore, and optional Prometheus metrics.
- A single static binary for Linux amd64, arm64 and armv7. Deploy it with
  hardened systemd units, in a Proxmox LXC container, or as a distroless
  Docker image.

Not included (for now): a DHCP server, serving DoH/DoT to clients, local
DNSSEC validation. TLS interception of downloads is never done.

## Screenshots

*Screenshots will be added here.*

<!--
![Overview](docs/screenshots/overview.png)
![Query log](docs/screenshots/query-log.png)
![Downloads](docs/screenshots/downloads.png)
-->

## Quick start

Build it (Go 1.27, Node.js 22, GNU make):

```sh
git clone https://github.com/hustenreizjuengling/picache.git
cd picache
make              # bin/picache for this machine
make build-all    # bin/picache-linux-{amd64,arm64,armv7}
```

**Debian 12/13 (bare metal, VM, Raspberry Pi)**

```sh
sudo sh deploy/install.sh --binary bin/picache-linux-amd64   # or -arm64 / -armv7
sudo picache setup-token
# open http://<ip>:8080/ and create the admin account
```

**Proxmox VE (unprivileged LXC):** create a Debian 12/13 container with
`nesting=1` and a static IP, copy the binary and `deploy/` into it and run
the same installer. See [deploy/lxc/README.md](deploy/lxc/README.md), which
also covers NAS mounts.

**Docker (host networking)**

```sh
cd deploy/docker
docker compose up -d --build
docker exec -u 65532:65532 picache /picache setup-token
# open http://<host-ip>:8080/
```

Then point your router's DHCP DNS option at PiCache. If port 53 is already
in use (for example by systemd-resolved), see
[Port 53 conflicts](docs/DEPLOYMENT.md#port-53-conflicts).

## Documentation

- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md): installation, first-run setup,
  backup and restore, upgrades, storage, environment variables, CLI.
- [deploy/lxc/README.md](deploy/lxc/README.md): Proxmox LXC and NAS.
- [docs/SECURITY.md](docs/SECURITY.md): threat model, hardening checklist,
  reporting vulnerabilities.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): design and specification.
- [docs/API.md](docs/API.md): REST API.

## Development

```sh
make web     # build the UI into internal/webui/dist (or: cd web && npm run dev)
make build   # bin/picache
make test vet lint
bin/picache serve --dev --data-dir ./data --cache-dir ./cache \
  --dns-listen 127.0.0.1:1053 --cache-listen off --sni-listen off \
  --web-listen 127.0.0.1:8080 --web-tls-listen off
```

The UI development server (`npm run dev` in `web/`) proxies `/api` to
`127.0.0.1:8080`. `scripts/test-install.sh bin/picache-linux-amd64` runs the
installer smoke test in a throwaway Debian container (needs Docker).

## Kurzüberblick (Deutsch)

PiCache ist ein filternder DNS-Server und ein LanCache-kompatibler
Download-Cache in einem einzigen Programm mit Weboberfläche (Deutsch und
Englisch).

- **DNS-Filter** wie Pi-hole/AdGuard Home: Blocklisten, eigene Regeln,
  Gruppen pro Client, lokale DNS-Einträge, verschlüsselte Upstreams
  (DoH/DoT), Abfrageprotokoll und Statistiken.
- **Download-Cache** für Spiele und Updates (Steam, Epic, Battle.net, Riot,
  Xbox, Windows Update, PlayStation, Nintendo, …): Inhalte werden nach dem
  ersten Download aus dem lokalen Netz geliefert, wahlweise von einer lokalen
  SSD oder einem NAS (SMB/NFS). Die Oberfläche zeigt, was heruntergeladen
  wurde und wie viel Bandbreite gespart wurde.
- **Sicher voreingestellt:** kein offener Resolver, unprivilegierter Dienst,
  Einrichtung per Einmal-Token, optionale Zwei-Faktor-Anmeldung.
- **Betrieb** auf Debian 12/13 (auch Raspberry Pi 4/5), in einem Proxmox-LXC
  oder mit Docker.

Schnellstart auf Debian: `sudo sh deploy/install.sh --binary <datei>`, dann
`sudo picache setup-token` ausführen und `http://<ip>:8080/` öffnen.
Anschließend im Router (DHCP) die IP von PiCache als DNS-Server eintragen.
Auf einem Raspberry Pi gehört der Cache auf eine USB-SSD, nicht auf die
SD-Karte. Details stehen in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
(englisch).
