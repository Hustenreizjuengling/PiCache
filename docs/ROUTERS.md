# Router set-up

PiCache filters only the queries it gets. The router decides which DNS
server the devices use: it hands it out with the addresses (DHCP) and, for
IPv6, announces it (router advertisements or DHCPv6). This page has the
steps for common routers; the FRITZ!Box steps are in
[DEPLOYMENT.md](DEPLOYMENT.md#router-set-up-fritzbox), and **DNS → Network
check** in the web UI shows the steps for your router with PiCache's
addresses filled in and checks the result.

The menu names change between firmware versions. Everything below was
**not verified on a real device** unless it says so; corrections are
welcome.

## What every router needs

1. **The DNS server of the LAN's DHCP server** is PiCache's IPv4 address
   (a fixed address: [DEPLOYMENT.md](DEPLOYMENT.md#first-run-setup)). Not the
   router's own address, and not PiCache as the router's upstream DNS
   server: then the router forwards every query and PiCache sees only the
   router (the network check warns *router-forwarding*).
2. **IPv6**: the router announces PiCache's **ULA** (the `fd…` address the
   network check shows; give PiCache one if it has none) as DNS server, by
   RDNSS in its router advertisements or by DHCPv6, or it announces no IPv6
   DNS server at all. Never a global address (`2…` or `3…`): it changes with
   the provider's prefix.
3. **Rebind protection** of the router: keep it on. If the router itself
   still resolves names through PiCache (its own upstream, a forwarded
   domain), PiCache's answers with private addresses would be dropped by it:
   add exceptions per domain only (your local domain and, where the router
   forwards to PiCache, the domains of the download cache), never "turn
   rebind protection off".
4. **No loop**: when the router's own upstream (WAN) DNS server is PiCache
   while PiCache asks the router for local names (**DNS settings → Router
   resolver**, `dns.routerResolver`, or **Local PTR upstreams**,
   `dns.localPtrUpstreams`), local names go round in circles. Keep the
   router's WAN DNS at the provider's or a public resolver, or remove the
   router from those two settings.

A router that cannot change the DNS server it hands out (many Telekom
Speedport and Vodafone Station models): switch its DHCP server off and let
PiCache hand out the addresses ([DEPLOYMENT.md](DEPLOYMENT.md#dhcp-server)).

Devices pick up the new DNS server when they renew their address or
reconnect (switching Wi-Fi off and on is enough).

---

## OPNsense

- **IPv4**: *Services → ISC DHCPv4 → [LAN] → DNS servers*: PiCache's IPv4
  address (with Kea: *Services → Kea DHCP → Kea DHCPv4 → Subnets → DNS
  servers*; with Dnsmasq DHCP: *Services → Dnsmasq DNS & DHCP → DHCP
  options*, option `dns-server`).
- **IPv6**: *Services → Router Advertisements → [LAN]*: under *DNS servers*
  PiCache's ULA, and switch off *Use the DNS configuration of the DHCPv6
  server* if you do not serve DHCPv6; with DHCPv6: *Services → ISC DHCPv6 →
  [LAN] → DNS servers*.
- **Rebind**: *Services → Unbound DNS → Advanced → Private Domains*: your
  local domain (and the download cache domains if Unbound forwards them to
  PiCache).
- **Loop**: *System → Settings → General → DNS servers* stay at the
  provider's or public resolvers.

## pfSense

- **IPv4**: *Services → DHCP Server → LAN → DNS Servers*: PiCache's IPv4
  address.
- **IPv6**: *Services → Router Advertisement → LAN → DNS Servers*: PiCache's
  ULA (and *Services → DHCPv6 Server → LAN → DNS Servers* when DHCPv6 runs).
- **Rebind**: *Services → DNS Resolver → General Settings → Custom options*:
  `private-domain: "<local domain>"` per domain; the forwarder (dnsmasq)
  has *DNS Rebind Check* exceptions as `rebind-domain-ok=/<domain>/`.
- **Loop**: *System → General Setup → DNS Servers* stay at the provider's or
  public resolvers.

## OpenWrt

With LuCI (*Network → Interfaces → lan → DHCP Server*) or on the command
line:

```sh
uci add_list dhcp.lan.dhcp_option='6,<PiCache IPv4>'     # IPv4 DNS server for the LAN
uci add_list dhcp.lan.dns='<PiCache ULA>'                # IPv6: announced by odhcpd (RA and DHCPv6)
uci add_list dhcp.@dnsmasq[0].rebind_domain='<local domain>'
uci commit dhcp && service dnsmasq restart && service odhcpd restart
```

- In LuCI the IPv4 option is *Advanced Settings → DHCP-Options*, the IPv6
  server *IPv6 Settings → Announced IPv6 DNS servers*, the rebind exception
  *Network → DHCP and DNS → Filter → Domain whitelist*.
- **Loop**: the router's own upstream (*Network → Interfaces → wan →
  Advanced Settings → Use custom DNS servers*) stays at the provider's or
  public resolvers.

## ASUS (Asuswrt and Asuswrt-Merlin)

- **IPv4**: *LAN → DHCP Server → DNS Server 1*: PiCache's IPv4 address, and
  *Advertise router's IP in addition to user-specified DNS*: **No** (else
  devices also ask the router).
- **IPv6**: *IPv6 → IPv6 LAN Setting → DNS Server 1*: PiCache's ULA (with
  *Connect to DNS Server automatically* off). Older stock firmware may not
  hand out a ULA; Asuswrt-Merlin does.
- **Rebind**: Asuswrt-Merlin: *LAN → DHCP Server* (dnsmasq) custom options
  `rebind-domain-ok=/<local domain>/` in `/jffs/configs/dnsmasq.conf.add`;
  keep *Prevent DNS rebind attacks* on.
- **Loop**: *WAN → Internet Connection → DNS Server* stays automatic (the
  provider) or public.

## TP-Link (Archer and Deco)

- **Archer**: *Advanced → Network → DHCP Server → Primary DNS*: PiCache's
  IPv4 address (*Secondary DNS* empty, or the second PiCache if you run two).
  IPv6: *Advanced → IPv6 → LAN* offers no custom DNS server on many models;
  then switch IPv6 off on the LAN or let PiCache announce itself
  ([DEPLOYMENT.md](DEPLOYMENT.md#ipv6-announcements)).
- **Deco** (app): *More → Advanced → DHCP Server → DNS*: PiCache's IPv4
  address. The Deco hands out no custom IPv6 DNS server; same advice as for
  the Archer.
- No rebind settings. **Loop**: the WAN DNS stays automatic.

## UniFi (Network application)

- **IPv4**: *Settings → Networks → [LAN] → DHCP Service Management → DNS
  Server*: *Manual*, PiCache's IPv4 address.
- **IPv6**: *Settings → Networks → [LAN] → IPv6 → DNS Server*: *Manual*,
  PiCache's ULA (announced in the router advertisements).
- Switch *Content Filtering* and *Ad Blocking* of the gateway off for this
  network (they redirect DNS).
- **Loop**: *Settings → Internet → [WAN] → DNS Server* stays automatic or
  public.

## Telekom Speedport

Most Speedport models (Speedport Smart 3, Smart 4 and older) cannot change
the DNS server their DHCP server hands out. Switch the Speedport's DHCP
server off (*Heimnetz → Heimnetzwerk (LAN) → Heimnetzwerk → DHCP*) and let
PiCache's [DHCP server](DEPLOYMENT.md#dhcp-server) hand out the addresses
(it announces itself as DNS server, for IPv6 too when its router
advertisements are on). Where a model has the option, it is under
*Heimnetz → Heimnetzwerk (LAN) → Name und Adresse des Routers → Lokaler
DNS-Server*.

## Vodafone Station

The Vodafone Station (Technicolor and Sagemcom models) cannot change the
DNS server of its DHCP server. Switch its DHCP server off (*Einstellungen →
Lokales Netzwerk → DHCP*, "Expert mode") and use PiCache's
[DHCP server](DEPLOYMENT.md#dhcp-server) as for the Speedport.
