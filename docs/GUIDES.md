# Guides

Step-by-step set-ups around PiCache. Every command is meant to be copied as
it is; replace the placeholders in angle brackets (`<LAN-CIDR>`, `<PiCache
IP>`, …). Steps that were not tried on a real device or installation are
marked *not verified on a real device*.

Contents: [Moving from another DNS filter](#moving-from-another-dns-filter) ·
[Unbound as a local recursive resolver](#unbound-as-a-local-recursive-resolver) ·
[Filtering away from home](#filtering-away-from-home) ·
[Home Assistant](#home-assistant) · [Firewall rules](#firewall-rules)

See also [DEPLOYMENT.md](DEPLOYMENT.md) (installation),
[ROUTERS.md](ROUTERS.md) (router settings) and [DEVICES.md](DEVICES.md)
(single devices).

---

## Moving from another DNS filter

PiCache cannot read another filter's database, but everything such a filter
holds can be exported as text and pasted into PiCache's importers. Every
importer shows a **preview** first and saves **all or nothing**: one line
with an error (the preview names the line and the reason) means nothing is
saved until you fix or remove it (the rule import offers **Remove the lines
with errors**). Query history and statistics do not move over.

### What goes where

| In the old filter | Typical export | In PiCache |
|---|---|---|
| Subscribed block and allow lists | their URLs | **Filtering → Blocklists → Add blocklist** / **Add allowlist**, or the catalogue ([below](#2-lists)) |
| Own block and allow rules, block and allow domain lists | adblock-style lines (`\|\|example.com^`, `@@\|\|example.com^`), domain names, hosts lines | **Filtering → Rules → Import** |
| Regular-expression lists | one expression per line | **Filtering → Rules → Import**, each wrapped in `/…/` |
| Blocked answer addresses | addresses and networks | **Filtering → Rules → Rules for answer addresses** (by hand), or a list with the content *Answer IP addresses* |
| Local DNS records | a hosts file, or `address=/name/ip` lines | **Local DNS → Records → Import hosts file** |
| CNAME and wildcard records | `cname=` lines, `address=/name/ip` for a name and its subdomains | **Local DNS → Records → Add record** (by hand) |
| Conditional forwarding | `server=/domain/ip` lines, or a setting "network, router, local domain" | **Local DNS → Forwarders → Import**, or the router resolver ([below](#5-conditional-forwarding)) |
| Clients and groups, per-client rules | a list of names, addresses and MAC addresses | **Clients & groups** (by hand or through the API) |
| DHCP reservations | `dhcp-host=` lines, a CSV or hosts file | **DNS → DHCP → Reserved addresses → Import** |
| Upstream servers, blocking reply, retention | settings | **DNS settings**, **System → Logs & privacy** (by hand) |

Work in this order: groups and clients first (the imports can assign rules
and records to groups), then lists, rules, local records, forwarders and
reservations, then the switch-over. The commands below are for a shell
with `sed` (any Linux or macOS machine, or Git Bash); check what they
write before you paste it. `<PiCache IP>` is PiCache's address.

### Before you start: where PiCache runs

- **On another machine or container** (the easy way): install PiCache
  there ([DEPLOYMENT.md](DEPLOYMENT.md)) and keep the old filter running
  until the [switch-over](#8-switch-over) is complete.
- **On the same machine:** both need port 53. The installer notices that
  port 53 is taken and installs PiCache without starting it. Let PiCache
  answer on a spare port while you move the configuration over: add
  `PICACHE_DNS_LISTEN=127.0.0.1:1053` to `/etc/picache/picache.env`, then
  `sudo systemctl start picache` (Docker: `PICACHE_DNS_LISTEN: "127.0.0.1:1053"`
  under `environment:`). If the old filter's web interface holds port 8080,
  open PiCache at `https://<PiCache IP>:8443/`.

Then complete the [first-run setup](DEPLOYMENT.md#first-run-setup).

### 1. Groups and clients

There is no import for clients. Create the groups first (**Clients &
groups → Groups → Add group**), then the clients (**Clients → Add
client**, or **Seen recently → Add as client** for devices that already
asked PiCache). A client is identified by IP addresses, networks (CIDR),
MAC addresses, `clientid:<id>` (encrypted DNS), `iface:<interface>` or
`host:<name>`. A client gets exactly the groups you give it: keep
**Default** among them if the Default group's lists and rules should
still apply to it. Devices you do not add are in Default.

Per-client rules of the old filter (rules that name a client or a client
tag) become groups: create a group for the devices, give them that group,
and import the rules for it (step 3). For a single device, **Allow only for
this device** / **Block only for this device** in a query log row creates a
group of its own and the rule for it.

Many clients can be created with an admin API token (**System → API
tokens**) from a tab-separated file of name, address, MAC address and
group id (`GET /api/v1/groups` lists the ids; the Default group is `1`).
The commands use HTTPS, so the token does not cross the network in clear
text: download PiCache's CA as `picache-ca.crt` under **System → HTTPS
certificate** first (with a certificate of your own, use its name and
leave out `--cacert`). Each line prints what it added and the HTTP status;
the loop stops at the first status other than 201 and shows the reply (a
307 means a plain `http://` address met **Redirect HTTP to HTTPS**):

```sh
export PICACHE_TOKEN=pc_...
while IFS="$(printf '\t')" read -r name ip mac group; do
  code=$(curl -sS --cacert picache-ca.crt -o reply.json -w '%{http_code}' \
    -H "Authorization: Bearer $PICACHE_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"identifiers\":[\"$ip\",\"$mac\"],\"groupIds\":[$group]}" \
    "https://<PiCache IP>:8443/api/v1/clients")
  echo "$name: $code"
  [ "$code" = 201 ] || { cat reply.json; echo; break; }
done < clients.tsv
```

### 2. Lists

The lists your old filter subscribes to work in PiCache as they are: hosts
files, domain lists and adblock-style lists (`$important`, `$badfilter`,
`$dnstype` and `$denyallow` are understood; lines with other modifiers are
counted as unsupported and skipped). Look for each list in the catalogue of
**Filtering → Blocklists** first (it keeps the list's category and
maintainer); add the others with **Add blocklist** or **Add allowlist**:
the address (https; plain http only for a private IP address), the groups,
and **Plain domain names**: whether a line that is only a name blocks just
that name (the default) or its subdomains too. HaGeZi Multi NORMAL is
subscribed on a new installation; disable it if you do not want it. At
most 100 lists.

A list file of your own can go to `<data>/lists/local/` (for example
`/var/lib/picache/lists/local/own.txt`, readable by the service user) and be
added as `file:///var/lib/picache/lists/local/own.txt`, or be imported as
rules (step 3). Many URLs at once, one per line in `lists.txt`, go to the
Default group with:

```sh
while read -r url; do
  code=$(curl -sS --cacert picache-ca.crt -o reply.json -w '%{http_code}' \
    -H "Authorization: Bearer $PICACHE_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"url\":\"$url\",\"enabled\":true}" "https://<PiCache IP>:8443/api/v1/filter/lists")
  echo "$url: $code"
  [ "$code" = 201 ] || { cat reply.json; echo; break; }
done < lists.txt
```

(`"kind":"allow"` adds an allowlist, `"groupIds":[1,2]` other groups.) An
allowlist never lifts parental controls or the lists of the category
switches (adult content, gambling, dating, piracy, bypass).

### 3. Own rules

**Filtering → Rules → Import** takes one rule per line and assigns the
rules to the groups you choose in the dialog:

| Line | Becomes |
|---|---|
| `example.com`, `\|example.com^` | block this name only |
| `\|\|example.com^`, `*.example.com` | block the name and its subdomains |
| `0.0.0.0 example.com tracker.example.com` (hosts) | block these names only |
| `/^ad[0-9]*\.example\.net$/` | block by a regular expression (RE2: no back-references) |
| `@@` before any of these but hosts lines | an allow rule |
| `$dnstype=AAAA`, `$denyallow=a.example.com`, `$important` | a rule for these query types, with exceptions; `$important` is accepted and dropped |
| `$dnsrewrite=NXDOMAIN` (`REFUSED`, `NOERROR`), `$dnsrewrite=192.0.2.10` | the rule's own reply |
| `/re/;querytype=A,AAAA`, `;invert`, `;reply=nxdomain` | the suffixes of regular-expression lists |
| lines starting with `!` or `#`, empty lines | skipped |

The preview refuses, and you convert:

- `$client=…` and `$ctag=…`: remove the modifier and import those lines
  for the group you created in step 1.
- `$badfilter`: drop the line and the line it cancels.
- `$dnsrewrite` to another name, or a CNAME: create a local record (step 4).
- `;reply=none` (no answer at all): add the name to **DNS settings →
  Protection → Dropped domains**.
- An address or a network as a rule: add it under **Rules for answer
  addresses** on the same page.
- URL rules and other modifiers (`$third-party`, …) have no meaning for DNS:
  drop them.

Converting the usual formats (check the output, then paste or load it):

```sh
# a regular-expression list: wrap every expression in /…/, keep ;querytype= and the like
sed -E -e '/^[[:space:]]*(#|$)/d' -e 's#^([^;]*)(;.*)?$#/\1/\2#' regex.list > regex-rules.txt
# a list of exact names to allow
sed -E -e '/^[[:space:]]*(#|$)/d' -e 's/^/@@/' allow.list > allow-rules.txt
# a list of names to block with their subdomains ("wildcard" lists)
sed -E -e '/^[[:space:]]*(#|$)/d' -e 's/.*/||&^/' wildcard.list > wildcard-rules.txt
```

A list of exact names to block needs no conversion. Your allow rules win
over every list; your block rules for exact names and subdomains win over
the lists too, your regular-expression block rules only over the lists'
own patterns. At most 20 000 lines per import, 20 000 rules, 1 000 of them
regular expressions. **Filtering → Why is this blocked?** tests a name for
a device and shows which rule or list decides.

### 4. Local DNS records

**Local DNS → Records → Import hosts file** turns every name of a hosts
file (`<address> <name> [<alias> …]`) into an A or AAAA record for everyone
or for groups you choose. Loopback lines and names like `localhost` are
skipped. Lines with `0.0.0.0` or `::` are refused: such a file is a
blocklist (step 2). Wildcard names (`*.apps.example.lan`), CNAME, TXT, SRV,
MX and other types are created with **Add record** (a wildcard matches the
subdomains only). PiCache also answers reverse lookups of the imported
addresses by itself.

`address=/name/address` lines (dnsmasq style) become a hosts file, and
their blocking forms (`address=/name/0.0.0.0`, `address=/name/`) become
rules:

```sh
sed -n -E 's#^address=/([^/]+)/([0-9a-fA-F.:]+)$#\2 \1#p' dnsmasq.conf |
  grep -v -E '^(0\.0\.0\.0|::) ' > records-hosts.txt        # Local DNS → Records → Import hosts file
sed -n -E 's#^address=/([^/]+)/(0\.0\.0\.0|::)?$#||\1^#p' dnsmasq.conf > address-rules.txt   # Filtering → Rules → Import
```

Such an `address=` line also answers every name below the name; add a
wildcard record `*.<name>` by hand where that is needed. Lines with
several names (`address=/a/b/…`) are not converted: split them first.

### 5. Conditional forwarding

**Local DNS → Forwarders → Import** takes one forwarder per line:
`[/domain1/domain2/]server1 server2 …` (up to 16 domains and 8 servers;
`#` as the server means PiCache's default upstreams, `[//]` the
single-label names that nothing local answers). Servers are written like
upstreams: `10.0.0.53`, `10.0.0.53:5353` or `10.0.0.53#5353`, `[fd00::53]`
(IPv6 in brackets), `tls://dns.example.net`, `https://…`. A port works in
both forms, so `server=/domain/address` lines (dnsmasq style) convert with:

```sh
sed -n -E 's#^server=(/.+/)([^/]*)$#[\1]\2#p' dnsmasq.conf > forwarders.txt
```

`server=/domain/` without a server (never forward) has no forwarder;
create local records instead.
`rev-server=192.168.1.0/24,192.168.1.1` becomes
`[/1.168.192.in-addr.arpa/]192.168.1.1`. Imported forwarders do not
validate DNSSEC; switch **Validate DNSSEC** on per forwarder where its
servers resolve public, signed zones.

**Asking the router for local names** (a setting "conditional forwarding"
with a network, the router's address and the local domain) usually needs
no forwarder: set **DNS settings → Local names → Local domain** to the
router's domain (for example `fritz.box`) and keep **Router resolver** on
automatic. PiCache then asks the router for names below the local domain,
for bare names such as `nas` and for reverse lookups of private
addresses. For a network that is not private, add it under **More private
networks for reverse lookups**; for another server, import a forwarder.

### 6. DHCP reservations

Only if the old filter handed out addresses: **DNS → DHCP → Reserved
addresses → Import** takes a CSV (`mac,ip,hostname,comment`), a hosts file with the MAC
address as comment (`ip name # mac`) or lines `mac ip [hostname]`, with a
preview. `dhcp-host=mac,ip[,name]` lines (dnsmasq style) convert with:

```sh
sed -n -E 's#^dhcp-host=([0-9A-Fa-f:]{17}),([0-9.]+)(,([A-Za-z0-9-]+))?$#\1 \2 \4#p' dnsmasq.conf > reservations.txt
```

Other forms (with a lease time, a tag or a client identifier) are skipped
by this command; add them by hand. Set up the range and switch the old DHCP
server off before you switch PiCache's on: PiCache refuses to serve while
another DHCP server answers ([DHCP server](DEPLOYMENT.md#dhcp-server)).

### 7. Settings

Upstream servers (**DNS settings → Upstream DNS servers**: `9.9.9.9`,
a local resolver as `127.0.0.1#5335` or `127.0.0.1:5335`, `tls://…`,
`https://…`, `quic://…`, `sdns://…`), the reply for blocked
names (**DNS settings → Blocking**), the rate limit (**DNS settings →
Rate limit**), the access list (**DNS settings → Access**) and the query log retention and privacy
(**System → Logs & privacy**) are set by hand. The defaults are a good
start: Quad9 over DNS-over-HTTPS, DNSSEC validation, blocked names
answered with `0.0.0.0`/`::`, 7 days of query log.

### 8. Switch-over

1. Test PiCache before any device depends on it: `dig @<PiCache IP>
   example.com` (on the same machine `dig @127.0.0.1 -p 1053 example.com`),
   a name you block, a local name and a forwarded name.
2. Point one device at PiCache by hand and watch it in the **Query log**.
3. On the same machine: stop and disable the old filter, remove the
   `PICACHE_DNS_LISTEN` line and restart PiCache (`sudo systemctl restart
   picache`). If PiCache now answers on the old filter's address, the
   router needs no change.
4. Otherwise set the DNS server of the router's DHCP server to PiCache
   ([ROUTERS.md](ROUTERS.md)), IPv6 included.
5. Keep the old filter running (on its own machine) until **DNS → Network
   check** shows your devices asking PiCache: devices move when they renew
   their lease or reconnect. Then switch it off.
6. Switch on scheduled backups (**System → Backup & restore**).

---

## Unbound as a local recursive resolver

PiCache forwards the queries it does not answer itself to upstream DNS
servers (Quad9 by default). With [Unbound](https://nlnetlabs.nl/projects/unbound/)
on the same machine, the queries are resolved from the root servers
instead: no single upstream operator sees all of your queries. The price:
the first query of a name is slower (Unbound walks from the root), and
Unbound must be reachable and correct, or nothing resolves.

### 1. Write Unbound's configuration first

Debian's `unbound` package starts Unbound as soon as it is installed. Without
a configuration it listens on `127.0.0.1:53` and `[::1]:53`, and at the next
restart or boot of PiCache that port is taken: PiCache's DNS listener cannot
bind and PiCache stops. So write the configuration **before** installing the
package (it only listens on port 5335 of the loopback interface, nothing
else can reach it):

```sh
sudo install -d -m 0755 /etc/unbound/unbound.conf.d
sudo tee /etc/unbound/unbound.conf.d/picache.conf >/dev/null <<'EOF'
server:
    interface: 127.0.0.1
    interface: ::1
    port: 5335
    access-control: 127.0.0.0/8 allow
    access-control: ::1/128 allow
    do-ip4: yes
    do-ip6: yes
    do-udp: yes
    do-tcp: yes
    harden-glue: yes
    harden-dnssec-stripped: yes
    qname-minimisation: yes
    edns-buffer-size: 1232
    prefetch: yes
EOF
sudo apt install unbound
sudo systemctl disable --now unbound-resolvconf.service   # it rewrites the host's /etc/resolv.conf
```

Instead of writing the file first you can keep Unbound from starting during
the installation: `sudo systemctl mask unbound`, `sudo apt install unbound`,
write the file, then `sudo systemctl unmask unbound && sudo systemctl
restart unbound`.

Other distributions (the steps are the same; *not verified on a real
device*): Fedora and RHEL `sudo dnf install unbound` (the file goes to
`/etc/unbound/conf.d/picache.conf`, and the service is not started until
`sudo systemctl enable --now unbound`), Arch `sudo pacman -S unbound`,
openSUSE `sudo zypper install unbound` (on both put the `server:` lines into
`/etc/unbound/unbound.conf`, then `sudo systemctl enable --now unbound`).

Check that Unbound answers on its port and that nothing but PiCache holds
port 53:

```sh
dig @127.0.0.1 -p 5335 example.com      # status: NOERROR, and the "ad" flag for signed names
ss -lnup 'sport = :53'                   # only picache, no unbound
```

(`dig` is in the package `dnsutils` on Debian and Ubuntu, `bind-utils` on
Fedora and RHEL, `bind` on Arch and `bind-utils` on openSUSE.)

### 2. Point PiCache at Unbound

Under **DNS → DNS settings → Upstreams** (or with `picache config set dns`):

- **Upstream DNS servers** (`dns.upstreams`): `udp://127.0.0.1:5335` only
  (`127.0.0.1#5335`, as other DNS filters' guides write it, works as well).
- **Mode** (`dns.upstreamMode`): `strict`.
- **DNSSEC mode** (`dns.dnssecMode`): `passthrough` (**Pass through**).
  PiCache then sets the DO bit and passes Unbound's AD flag through;
  Unbound validates the signatures, PiCache does not. `validate` works
  too, but checks every signature twice (Unbound and PiCache) and counts
  on Unbound returning the DNSSEC data, which it does.
- **Fallback DNS servers** (`dns.fallbackUpstreams`): an explicit choice.
  - `[]` (none) for privacy: nothing leaves the house except Unbound's own
    recursion. If Unbound fails, nothing resolves. Empty the **Bootstrap
    servers** (`dns.bootstrap: []`) too, unless a client group uses a
    family resolver: while PiCache's clock guard is active (the clock is
    before the program's build date) it asks the bootstrap servers.
  - Kept for availability: the fallback operator then gets the queries
    whenever Unbound does not reply at all (a timeout, a crash). PiCache
    never asks a fallback after a reply, SERVFAIL included.

### 3. Keep the clock right without PiCache

Unbound validates DNSSEC, and signatures have validity dates. A machine
whose clock is wrong (a Raspberry Pi without a battery-backed clock, right
after a boot) makes Unbound answer SERVFAIL for every signed name, and
PiCache does not ask the fallbacks after a SERVFAIL. If the machine then
needs DNS to set its clock (an NTP server given by name), it never
recovers. So the time sync must not depend on PiCache:

```sh
sudo install -d /etc/systemd/timesyncd.conf.d
printf '[Time]\nNTP=<router IP> 194.58.200.20 162.159.200.123\n' | sudo tee /etc/systemd/timesyncd.conf.d/picache.conf
sudo systemctl restart systemd-timesyncd
timedatectl timesync-status                   # Server: one of the addresses above
```

The addresses are your router's (if it serves NTP) and public NTP servers
given by IP address (here `194.58.200.20` of the Swedish Netnod, and
`162.159.200.123` of Cloudflare); pick ones near you. On Raspberry Pi OS,
`sudo apt install fake-hwclock` also restores the last known time at boot.

### Docker

With host networking (`deploy/docker/docker-compose.yml`) the container
reaches Unbound on the host's `127.0.0.1:5335` as written above. In a bridge
network it does not: let Unbound listen on the address of the Docker bridge
gateway (`docker network inspect <network>`, e.g. `172.18.0.1`) and allow
exactly that network, and use that address as PiCache's upstream:

```
server:
    interface: 172.18.0.1
    port: 5335
    access-control: 172.18.0.0/16 allow
```

---

## Filtering away from home

PiCache answers only the networks of its access lists; it is never a DNS
server for the Internet. Devices away from home use it through a VPN that
ends in your home network. **Never**:

- forward ports 53, 853, 443, 8080 or 8443 on the router;
- switch on **Allow all networks** (`dns.allowAllNetworks`);
- switch **Restrict the web UI to allowed networks** (`web.restrictToNetworks`)
  off for remote use.

Encrypted DNS (DoT, DoH, [DEPLOYMENT.md](DEPLOYMENT.md#encrypted-dns)) from
outside only goes through the VPN too; a ClientID then tells the devices
apart.

### WireGuard

A client configuration for `wg-quick` (the phone and desktop apps import the
same format) that sends DNS to PiCache:

```ini
[Interface]
PrivateKey = <client private key>
Address = 10.8.0.2/32
DNS = <PiCache LAN IP>

[Peer]
PublicKey = <server public key>
Endpoint = <your home address or DynDNS name>:51820
# Split tunnel: only the home network and the tunnel go through the VPN.
# PiCache's address must be inside AllowedIPs.
AllowedIPs = 192.168.1.0/24, 10.8.0.0/24
```

- A tunnel network in private address space (`10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16`, like `10.8.0.0/24` above) is allowed by PiCache by
  default. For any other tunnel network add exactly its CIDR under **DNS →
  DNS settings → Access → Additional networks** (`dns.allowedNetworks`).
- The WireGuard server must route the tunnel into the LAN without NAT
  (masquerade), or every query comes from the WireGuard server's address and
  PiCache cannot tell the devices apart.

### Tailscale

1. Install Tailscale on the PiCache machine and note its tailnet address
   (`tailscale ip -4`, a `100.x.y.z` address).
2. In the admin console under **DNS**: add that address as a **global
   nameserver** and switch on **Override local DNS**. This works with
   MagicDNS on or off. With a subnet router into your LAN, PiCache's LAN
   address works as the nameserver too.
3. `100.64.0.0/10`, the tailnet's range, is allowed for DNS and the web UI
   by default. That means every node that can reach PiCache in the tailnet,
   devices shared in from other tailnets included, can use its DNS and reach
   its sign-in page. Restrict PiCache's ports to your own devices with an
   access rule, for example in the tailnet policy file (tag the PiCache node
   `tag:picache`):

   ```json
   {
     "tagOwners": { "tag:picache": ["autogroup:admin"] },
     "grants": [
       {
         "src": ["autogroup:member"],
         "dst": ["tag:picache"],
         "ip": ["udp:53", "tcp:53", "tcp:853", "tcp:8080", "tcp:8443"]
       }
     ]
   }
   ```

   `autogroup:member` are the users of your own tailnet (not those who got
   the node shared). Keep a rule for your own access to the machine (ssh)
   as well; *not verified on a real device*.

---

## Home Assistant

Home Assistant reads PiCache's state through the REST API and switches
blocking and the parental overrides. Create **two API tokens** under
**System → API tokens** and keep both in `secrets.yaml`:

- a **read** token for the sensors (they use only read routes: `GET
  /system/overview`, `GET /dns/blocking`, `GET /parental/groups`, `GET
  /stats/summary`);
- an **admin** token only for the switches. There is no narrower scope: an
  admin token can change every setting, so treat it like the admin password
  ([SECURITY.md](SECURITY.md)).

```yaml
# secrets.yaml
picache_read_token: "Bearer pc_…"
picache_admin_token: "Bearer pc_…"
```

**Transport.** Never set `verify_ssl: false`. HTTPS needs a certificate that
Home Assistant already trusts (a Let's Encrypt certificate,
[DEPLOYMENT.md](DEPLOYMENT.md#lets-encrypt)): its REST integration takes no
CA file, so PiCache's local CA does not work. Otherwise use plain
`http://<PiCache IP>:8080` on the LAN, knowing that the tokens then cross
the LAN in clear text. Home Assistant's address must pass PiCache's web
access list (a private address does by default).

```yaml
# configuration.yaml
rest:
  - resource: http://<PiCache IP>:8080/api/v1/stats/summary?range=24h
    headers:
      Authorization: !secret picache_read_token
    scan_interval: 300
    sensor:
      - name: PiCache queries 24h
        value_template: "{{ value_json.dnsQueries }}"
      - name: PiCache blocked 24h
        value_template: "{{ value_json.dnsBlocked }}"
      - name: PiCache blocked percent 24h
        value_template: "{{ value_json.blockedPercent | round(1) }}"
        unit_of_measurement: "%"
  - resource: http://<PiCache IP>:8080/api/v1/system/overview
    headers:
      Authorization: !secret picache_read_token
    scan_interval: 60
    binary_sensor:
      - name: PiCache healthy
        value_template: "{{ value_json.health.ok }}"
    sensor:
      - name: PiCache queries per second
        value_template: "{{ value_json.dns.qps | round(1) }}"

switch:
  # On: blocking resumes. Off: blocking pauses for 15 minutes (never a
  # permanent disable).
  - platform: rest
    name: PiCache blocking
    resource: http://<PiCache IP>:8080/api/v1/dns/blocking
    method: post
    headers:
      Authorization: !secret picache_admin_token
      Content-Type: application/json
    body_on: '{"enabled": true}'
    body_off: '{"enabled": false, "pauseSeconds": 900}'
    is_on_template: "{{ value_json.enabled }}"
```

**Parental override of a group** (the group id is in the address of the
group's page under **DNS → Parental controls**; here `2`):

```yaml
rest_command:
  picache_kids_block_hour:
    url: http://<PiCache IP>:8080/api/v1/parental/groups/2/override
    method: put
    headers:
      Authorization: !secret picache_admin_token
    content_type: application/json
    payload: '{"mode": "block", "minutes": 60}'
  picache_kids_override_end:
    url: http://<PiCache IP>:8080/api/v1/parental/groups/2/override
    method: delete
    headers:
      Authorization: !secret picache_admin_token

rest:
  - resource: http://<PiCache IP>:8080/api/v1/parental/groups
    headers:
      Authorization: !secret picache_read_token
    scan_interval: 60
    binary_sensor:
      - name: PiCache kids blocked
        value_template: >-
          {{ (value_json | selectattr('groupId', 'eq', 2) | map(attribute='state') | first).blockAll }}
```

`{"mode": "allow", "minutes": 60}` lifts the group's blocked services and
schedules for an hour instead. *Not verified on a real device.*

**Notifications to Home Assistant.** Add a webhook channel under **System →
Notifications** with the URL `http://<Home Assistant IP>:8123/api/webhook/<random id>`
(an IP address: PiCache resolves no `.local` mDNS names). The id is a secret
(generate it with `openssl rand -hex 16`): anyone who knows it can trigger
the automation, and PiCache shows the URL path in the channel and in the
audit log. The automation:

```yaml
automation:
  - alias: PiCache notification
    triggers:
      - trigger: webhook
        webhook_id: "<random id>"
        allowed_methods: [POST]
        local_only: true
    actions:
      - action: persistent_notification.create
        data:
          title: "{{ trigger.json.title }}"
          message: "{{ trigger.json.message }}"
```

The webhook body is `{event, severity, title, message, time, instance,
hostname, version}` ([API.md](API.md#notifications--routes_notifygo)).

**Optional: Prometheus.** A Prometheus server can scrape `/metrics` (not
Home Assistant). It needs an API token with **Read only** access (create one
with an expiry under **System → API tokens**; an admin token works too, but
whoever reads the file could then change the settings) and the setting
**System → API tokens → Prometheus metrics** (`web.metricsEnabled`), which
is off by default.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: picache
    scheme: http
    authorization:
      credentials_file: /etc/prometheus/picache-token
    static_configs:
      - targets: ["<PiCache IP>:8080"]
```

---

## Firewall rules

Two different jobs: rules **on the PiCache machine** protect PiCache itself;
rules **on the router** keep devices from bypassing it. Rules on the PiCache
machine cannot do the second: PiCache is not in the path of a device that
asks another DNS server.

### On the PiCache machine

Open only what PiCache uses, only for your LAN, and keep ssh reachable. The
ports (the effective ones are listed under **System → Network →
Listeners**):

| Port | When |
|---|---|
| 53 udp and tcp | always (DNS) |
| 8080 and 8443 tcp | always (web UI; LAN only) |
| 80 and 443 tcp | with the download cache on |
| 853 tcp | with DoT on |
| the port of `PICACHE_DOH_LISTEN` | only when it is set |
| 67 udp and 547 udp | while the DHCP server is on (clients have no address yet: allow them on the LAN interface, not by source) |
| 123 udp | with the NTP server on |

Replace `<LAN-CIDR>` with your network, e.g. `192.168.1.0/24` (the
installer prints this machine's address and prefix when a firewall is
active). IPv6 clients need the same rules for your ULA prefix
(`fd00::/64`-like). Every snippet keeps loopback, established connections
and ssh open **before** anything is dropped, and allows the IPv6 neighbour
discovery and router messages without which IPv6 stops working.

**Always apply firewall changes in a way that undoes itself** if you lock
yourself out: each tool below has a way.

#### nftables

Its own table (never `flush ruleset`: that also removes the rules of Docker,
firewalld and libvirt). This table drops everything else that arrives at the
machine: add rules for its other services.

```sh
sudo tee /etc/nftables-picache.nft >/dev/null <<'EOF'
table inet picache
delete table inet picache
table inet picache {
  chain input {
    type filter hook input priority filter; policy drop;
    iif "lo" accept
    ct state established,related accept
    ct state invalid drop
    tcp dport 22 accept
    icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit, nd-router-advert, echo-request } accept
    icmp type echo-request accept
    udp sport 67 udp dport 68 accept      # this machine's own DHCP client
    udp sport 547 udp dport 546 accept    # its DHCPv6 client
    ip saddr <LAN-CIDR> udp dport 53 accept
    ip saddr <LAN-CIDR> tcp dport { 53, 8080, 8443 } accept
    # with the download cache on:
    # ip saddr <LAN-CIDR> tcp dport { 80, 443 } accept
    # with DoT on:
    # ip saddr <LAN-CIDR> tcp dport 853 accept
    # with PICACHE_DOH_LISTEN set:
    # ip saddr <LAN-CIDR> tcp dport <DoH port> accept
    # while the DHCP server is on:
    # iifname "<LAN interface>" udp dport { 67, 547 } accept
    # with the NTP server on:
    # ip saddr <LAN-CIDR> udp dport 123 accept
  }
}
EOF
sudo nft -c -f /etc/nftables-picache.nft                    # check only
echo 'nft delete table inet picache' | sudo at now + 5 minutes  # the undo, in case you lock yourself out
sudo nft -f /etc/nftables-picache.nft
sudo atq                                                     # still reachable? then cancel the undo:
sudo atrm <job number>
```

(`at` is in the package `at`; make the table permanent by including the
file from `/etc/nftables.conf` and enabling `nftables.service`.)

#### iptables

Its own chains, hooked into `INPUT` once. `iptables-apply` reverts unless
you confirm within 60 seconds:

```sh
sudo tee /root/picache-iptables.sh >/dev/null <<'EOF'
set -e
for t in iptables ip6tables; do
  $t -N PICACHE-IN 2>/dev/null || $t -F PICACHE-IN
  $t -A PICACHE-IN -i lo -j ACCEPT
  $t -A PICACHE-IN -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  $t -A PICACHE-IN -p tcp --dport 22 -j ACCEPT
done
ip6tables -A PICACHE-IN -p ipv6-icmp -j ACCEPT
iptables -A PICACHE-IN -p icmp --icmp-type echo-request -j ACCEPT
iptables -A PICACHE-IN -p udp --sport 67 --dport 68 -j ACCEPT    # this machine's own DHCP client
ip6tables -A PICACHE-IN -p udp --sport 547 --dport 546 -j ACCEPT # its DHCPv6 client
iptables -A PICACHE-IN -s <LAN-CIDR> -p udp --dport 53 -j ACCEPT
iptables -A PICACHE-IN -s <LAN-CIDR> -p tcp -m multiport --dports 53,8080,8443 -j ACCEPT
# with the download cache on:
# iptables -A PICACHE-IN -s <LAN-CIDR> -p tcp -m multiport --dports 80,443 -j ACCEPT
# with DoT on:
# iptables -A PICACHE-IN -s <LAN-CIDR> -p tcp --dport 853 -j ACCEPT
# while the DHCP server is on:
# iptables -A PICACHE-IN -i <LAN interface> -p udp --dport 67 -j ACCEPT
# with the NTP server on:
# iptables -A PICACHE-IN -s <LAN-CIDR> -p udp --dport 123 -j ACCEPT
iptables -A PICACHE-IN -j DROP
ip6tables -A PICACHE-IN -j DROP
iptables -C INPUT -j PICACHE-IN 2>/dev/null || iptables -A INPUT -j PICACHE-IN
ip6tables -C INPUT -j PICACHE-IN 2>/dev/null || ip6tables -A INPUT -j PICACHE-IN
EOF
sudo iptables-apply -t 60 -c 'sh /root/picache-iptables.sh'
```

(The IPv6 part allows only loopback, ssh and ICMPv6 here; add the DNS lines
with `ip6tables` and your ULA prefix for IPv6 clients.)

#### ufw

```sh
sudo ufw allow 22/tcp                                        # before enabling ufw
sudo ufw allow from <LAN-CIDR> to any port 53 proto udp
sudo ufw allow from <LAN-CIDR> to any port 53 proto tcp
sudo ufw allow from <LAN-CIDR> to any port 8080 proto tcp
sudo ufw allow from <LAN-CIDR> to any port 8443 proto tcp
# sudo ufw allow from <LAN-CIDR> to any port 80 proto tcp    # download cache
# sudo ufw allow from <LAN-CIDR> to any port 443 proto tcp   # download cache
# sudo ufw allow from <LAN-CIDR> to any port 853 proto tcp   # DoT
# sudo ufw allow in on <LAN interface> to any port 67 proto udp  # DHCP server
# sudo ufw allow from <LAN-CIDR> to any port 123 proto udp   # NTP server
echo 'ufw disable' | sudo at now + 5 minutes                # the undo
sudo ufw enable
sudo atq; sudo atrm <job number>                            # still reachable? cancel the undo
```

ufw allows ICMPv6 neighbour discovery by default (`/etc/ufw/before6.rules`).

#### firewalld

Rich rules in the zone of the LAN interface (`firewall-cmd
--get-zone-of-interface=<LAN interface>`); do not move the LAN into another
zone, that changes which rules apply to ssh. Rules without `--permanent`
are lost at the next reload, so try them at runtime first:

```sh
zone=$(firewall-cmd --get-zone-of-interface=<LAN interface>)
for rule in 'port port="53" protocol="udp"' 'port port="53" protocol="tcp"' \
    'port port="8080" protocol="tcp"' 'port port="8443" protocol="tcp"'; do
  sudo firewall-cmd --zone="$zone" --add-rich-rule="rule family=\"ipv4\" source address=\"<LAN-CIDR>\" $rule accept"
done
# with the download cache on, DoT on, DoH on its own port, the NTP server on:
# the same with port 80, 443, 853, the DoH port, 123 (udp)
# while the DHCP server is on: sudo firewall-cmd --zone="$zone" --add-port=67/udp --add-port=547/udp
# works? then keep it:
sudo firewall-cmd --runtime-to-permanent
```

A mistake is undone with `sudo firewall-cmd --reload` (from the console if
you are locked out) as long as you did not make it permanent.

#### Docker

Ports published by `docker-compose.bridge.yml` bypass the `INPUT` rules of
ufw and firewalld: Docker forwards them before `INPUT`. Restrict them in the
`DOCKER-USER` chain instead (host networking, the default compose file, is
covered by the rules above). `<LAN interface>` is the host's interface to
the LAN (`ip route show default` names it):

```sh
sudo iptables -I DOCKER-USER -i <LAN interface> -p udp -m conntrack --ctstate NEW --ctdir ORIGINAL --ctorigdstport 53 ! -s <LAN-CIDR> -j DROP
sudo iptables -I DOCKER-USER -i <LAN interface> -p tcp -m conntrack --ctstate NEW --ctdir ORIGINAL --ctorigdstport 53 ! -s <LAN-CIDR> -j DROP
sudo iptables -I DOCKER-USER -i <LAN interface> -p tcp -m conntrack --ctstate NEW --ctdir ORIGINAL --ctorigdstport 8080 ! -s <LAN-CIDR> -j DROP
```

(and the same for 8443, for 80 and 443 with the download cache on and for
853 with DoT on; *not verified on a real device*.) Keep `-i <LAN interface>`
and `--ctstate NEW`: `DOCKER-USER` sees every forwarded packet of every
container, and without them these rules also drop the containers' own
outgoing connections and their replies: PiCache's upstream queries (the
port 53 rules, so DNS fails for the whole network), and with the 80 and 443
rules its list downloads, cache fetches and DoH upstreams and every other
container's HTTP(S).

### On the router: keep devices from bypassing PiCache

Some devices (children's tablets, TVs, consoles) use their own DNS servers.
Only the router can stop that. Block outgoing DNS (tcp and udp port 53) and
DNS over TLS (port 853) to every destination **except PiCache**, and exempt
PiCache as the source, so that its own upstream queries still pass. Prefer
rejecting over redirecting: a device then falls back to the DNS server it
got by DHCP (PiCache).

**If you redirect instead** (DNAT to PiCache), never add SNAT or masquerade
for it: every redirected query would then come from the router, so per-client
groups and parental controls stop applying and the network check warns
*router-forwarding*. Redirecting without masquerade works only when PiCache
is in another network (VLAN) than the redirected devices; in the same
network PiCache's replies bypass the router and the devices discard them, so
reject there.

**DoH cannot be blocked by port** (it is HTTPS on 443). Switch on the
parental category **Bypass** (VPN, proxy and DoH services) for those groups
under **DNS → Parental controls**.

**OpenWrt** (fw4, `/etc/config/firewall`; *not verified on a real device*):

```sh
uci add firewall rule
uci set firewall.@rule[-1].name='Block-DNS-except-PiCache'
uci set firewall.@rule[-1].src='lan'
uci set firewall.@rule[-1].src_ip='!<PiCache IP>'
uci set firewall.@rule[-1].dest='wan'
uci set firewall.@rule[-1].dest_port='53 853'
uci set firewall.@rule[-1].proto='tcp udp'
uci set firewall.@rule[-1].target='REJECT'
uci commit firewall && service firewall restart
```

Devices can still ask the router's own resolver; that is fine while the
router hands out PiCache and forwards its own queries elsewhere.

**OPNsense and pfSense** (*Firewall → Rules → LAN*; *not verified on a real
device*): create an alias `PiCache` with PiCache's address, then, above the
default allow rule:

1. *Pass*, protocol TCP/UDP, source `PiCache`, destination any, destination
   port 53 (DNS), then the same for 853;
2. *Reject*, protocol TCP/UDP, source `LAN net`, destination *not*
   `PiCache`, destination port 53 (DNS), then the same for 853.

**Routers without such rules** cannot do this: most ISP routers (Telekom
Speedport, Vodafone Station) have no rules for outgoing traffic by port. The
FRITZ!Box can block "network applications" (a port list) per access profile
(*Internet → Filters → Lists → Network applications*) for the devices of a
profile; *not verified on a real device*.
