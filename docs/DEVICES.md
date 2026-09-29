# Setting up a device

Usually a device gets PiCache as its DNS server from the router
([ROUTERS.md](ROUTERS.md)) or from PiCache's own DHCP server, and needs no
setting at all. Set a device by hand when the router cannot do it, to test
PiCache on one device first, or to use encrypted DNS. The web UI has this
page with your addresses filled in: **DNS → Network check → Set up a
device**.

The values below are placeholders:

| Placeholder | What | Where the web UI takes it from |
|---|---|---|
| `<PiCache IPv4>` | PiCache's IPv4 address | the network check (this machine's addresses) |
| `<PiCache ULA>` | PiCache's unique local IPv6 address (`fd…`) | the same; without one there is **no stable IPv6 address: use IPv4 only** (a global IPv6 address changes with the provider's prefix) |
| `<DoT host name>` | the server name of DNS over TLS | **DNS settings → Encrypted DNS**, while DoT is on |
| `<DoH URL>` | `https://<server name>:8443/dns-query`: the port of the HTTPS web listener (8443 by default) or of `PICACHE_DOH_LISTEN`, left out only for 443 | the same, while DoH is on; **DNS settings → Encrypted DNS** and the web UI's device page show the exact URL |

Encrypted DNS needs a certificate the device trusts: a publicly trusted one
(Let's Encrypt), or PiCache's local CA installed on the device (**System →
HTTPS certificate** shows the steps per system to trust it; the
set-up of encrypted DNS is under **DNS settings → Encrypted DNS → Set up
devices**, [DEPLOYMENT.md](DEPLOYMENT.md#encrypted-dns)).

**Browsers** have their own "secure DNS" (DNS over HTTPS). While it is on
and points at another provider, the browser **bypasses PiCache** for web
pages: switch it off, or point it at `<DoH URL>`. Chrome and Edge: *Settings
→ Privacy and security → Security → Use secure DNS*; Firefox: *Settings →
Privacy & Security → DNS over HTTPS* (*Off*, or *Max Protection* with the
custom provider `<DoH URL>`). The parental category **Bypass** blocks the
known DoH providers for a group.

Contents: [Windows](#windows) · [macOS](#macos) · [iOS and iPadOS](#ios-and-ipados) ·
[Android](#android) · [Linux](#linux) · [ChromeOS](#chromeos) ·
[Game consoles](#game-consoles) · [Smart TVs and streaming boxes](#smart-tvs-and-streaming-boxes)

---

## Windows

**By hand** (Windows 11; Windows 10 is similar under *Control Panel →
Network and Sharing Center → Change adapter settings → Properties →
Internet Protocol Version 4/6*):

1. *Settings → Network & internet → Wi-Fi* (or *Ethernet*) *→ Hardware
   properties* (the network's properties) *→ DNS server assignment → Edit*.
2. *Manual*; switch **IPv4** on: *Preferred DNS* `<PiCache IPv4>`, no
   alternate DNS server (an alternate outside PiCache would be used
   sometimes, unfiltered).
3. Switch **IPv6** on: *Preferred DNS* `<PiCache ULA>` (without a ULA
   leave IPv6 off here).

**Encrypted DNS** (Windows 11): in the same dialog set *DNS over HTTPS* to
*On (manual template)* and enter `<DoH URL>` as the template, for IPv4 and
IPv6. Windows needs PiCache's certificate to be trusted.

Check: `nslookup example.com` shows `<PiCache IPv4>` as the server.

## macOS

**By hand**: *System Settings → Network → Wi-Fi* (or the Ethernet service)
*→ Details → DNS*: remove the other entries under *DNS Servers*, add
`<PiCache IPv4>` and `<PiCache ULA>`.

**Encrypted DNS**: install the configuration profile of **DNS settings →
Encrypted DNS → Set up devices → Apple configuration profile** (DoH or DoT,
optionally a ClientID and your home Wi-Fi names), then *System Settings →
General → Device Management* (*Profiles*), double-click it and install.

Check: `scutil --dns | grep nameserver` lists PiCache.

## iOS and iPadOS

**By hand** (per Wi-Fi network): *Settings → Wi-Fi → (i) next to the
network → Configure DNS → Manual*: delete the other servers, add
`<PiCache IPv4>` and `<PiCache ULA>`. Mobile data keeps the carrier's DNS.

**Encrypted DNS** (also on mobile data, with a VPN into your home network,
[GUIDES.md](GUIDES.md#filtering-away-from-home)): open the link of **Set up
devices → Apple configuration profile** on the device (or scan its QR code),
allow the download, then *Settings → General → VPN & Device Management*,
tap the profile and install it. It then appears under *Settings → General →
VPN & Device Management → DNS*.

## Android

**Encrypted DNS** (Android 9 or later) is the simplest way and covers
mobile data too (with a VPN into your home network): *Settings → Network &
internet → Private DNS → Private DNS provider hostname*: `<DoT host name>`
(or `<ClientID>.<DoT host name>` to tell the device apart). Android uses
port 853 only and needs a **publicly trusted** certificate (it does not use
user-installed CAs for this).

**By hand** (per Wi-Fi network, Android 10 and later; the menus differ
between manufacturers): *Settings → Network & internet → Internet → the
network's gear → Edit (pencil) → Advanced options → IP settings: Static*,
then *DNS 1* `<PiCache IPv4>` and *DNS 2* `<PiCache ULA>` (or empty). Static
IP settings also need a fixed address for the device; prefer Private DNS
or the router.

Note: Private DNS set to *Automatic* uses DoT with the network's DNS server
when it can, which works with PiCache while DoT is on.

## Linux

With NetworkManager (most desktops):

```sh
nmcli connection show                                       # the name of your connection
nmcli connection modify "<connection>" ipv4.dns "<PiCache IPv4>" ipv4.ignore-auto-dns yes
nmcli connection modify "<connection>" ipv6.dns "<PiCache ULA>" ipv6.ignore-auto-dns yes
nmcli connection up "<connection>"
```

**Encrypted DNS** with systemd-resolved (DoT; needs a trusted
certificate):

```sh
sudo install -d /etc/systemd/resolved.conf.d
printf '[Resolve]\nDNS=<PiCache IPv4>#<DoT host name>\nDNSOverTLS=yes\n' | sudo tee /etc/systemd/resolved.conf.d/picache.conf
sudo systemctl restart systemd-resolved
resolvectl status                                           # "DNSOverTLS" and the server
```

## ChromeOS

*Settings → Network → Wi-Fi → the network → Network → Name servers*:
*Custom name servers*, `<PiCache IPv4>` (and `<PiCache ULA>`).

**Encrypted DNS**: *Settings → Privacy and security → Security → Use secure
DNS*: *With* a custom provider, `<DoH URL>` (the certificate must be
trusted); switch it off otherwise, or ChromeOS uses its own provider.

## Game consoles

Consoles take a DNS server per network connection. Some games and apps use
hard-coded DNS servers anyway and bypass PiCache; only the router can stop
that ([GUIDES.md](GUIDES.md#firewall-rules)).

- **PlayStation 5**: *Settings → Network → Settings → Set Up Internet
  Connection → the network → Advanced Settings → DNS Settings: Manual*,
  *Primary DNS* `<PiCache IPv4>`, *Secondary DNS* `0.0.0.0` (none).
- **PlayStation 4**: *Settings → Network → Set Up Internet Connection →
  Custom*, *DNS Settings: Manual*, the same values.
- **Xbox Series X|S and Xbox One**: *Settings → General → Network settings →
  Advanced settings → DNS settings → Manual*: *Primary IPv4 DNS*
  `<PiCache IPv4>`, *Secondary* `<PiCache IPv4>` again (the Xbox asks for
  one).
- **Nintendo Switch**: *System Settings → Internet → Internet Settings →
  the network → Change Settings → DNS Settings: Manual*, *Primary DNS*
  `<PiCache IPv4>`, *Secondary DNS* empty.

Consoles speak IPv4 DNS here; none offers encrypted DNS. Game downloads
through PiCache's download cache work with any of these settings (the
names of the download services are answered by PiCache).

## Smart TVs and streaming boxes

The DNS setting is usually in the network settings under *IP settings* or
*Advanced*: set it to *Manual* and enter `<PiCache IPv4>` (and
`<PiCache ULA>` if the device takes IPv6). Examples:

- **Android TV / Google TV**: *Settings → Network & Internet → the network
  → IP settings: Static*, DNS 1 `<PiCache IPv4>` (static settings need a
  fixed address for the TV); newer versions have *Private DNS* like Android
  phones.
- **Apple TV**: *Settings → Network → Wi-Fi (or Ethernet) → the network →
  Configure DNS → Manual*: `<PiCache IPv4>`. Encrypted DNS needs the
  configuration profile, installed with Apple Configurator (*not verified
  on a real device*).
- **Fire TV**: *Settings → Network → forget and join the network again →
  Advanced*: *DNS 1* `<PiCache IPv4>`.
- **Samsung (Tizen), LG (webOS), Roku and others**: *Network → IP settings
  → DNS setting: Enter manually* (the path differs by model; Roku has no
  DNS setting at all).

Many TVs and apps use **hard-coded DNS servers** (often `8.8.8.8`) for
some of their traffic or ignore the setting completely; they then bypass
PiCache. Only rules on the router catch that
([GUIDES.md](GUIDES.md#firewall-rules)).
