# PiCache UI design system

**Subject.** A network appliance for a home lab or LAN party: the household's DNS and a download cache. **Audience.** The one person who runs the network: technical, but opening the UI between other things. **Primary job.** Answer "is everything OK, and what is my network doing?" in one glance, then let them drill into a domain, a client or a download in two clicks.

## Concept: the patch panel

The visual language borrows from structured cabling. Ethernet pairs follow the T568B colour code: blue, orange, green and brown. Those four pair colours are PiCache's categorical palette, and each has one fixed meaning everywhere (chips, charts, legends):

| Pair | Meaning | Light | Dark |
|---|---|---|---|
| Blue | DNS answered normally (allowed/forwarded/cached) | `#1f5fbf` | `#6ea4f2` |
| Orange | Blocked | `#c4591a` | `#f09a5b` |
| Green | Served from the cache (hit, bandwidth saved) | `#1e7f4f` | `#5cc98f` |
| Brown | Fetched from the Internet (WAN, cache miss) | `#7a4e2d` | `#c89b72` |

"Striped" variants (white stripe = the paired conductor) are used sparingly for *secondary* states of the same meaning, e.g. `cached` vs `forwarded` DNS answers, both blue; the stripe is a 45° repeating gradient on chips and a dashed stroke in charts.

The one memorable element is the **pair strip**: a 6 px bar along the top of the app shell, split into four segments whose widths are the live share of DNS allowed, blocked, cache hit and WAN traffic over the last 15 minutes. It doubles as a legend on hover. Everything else is quiet.

Pair strip formula: the DNS half and the cache half each take 50 % (100 % if the other half had no traffic in the window). DNS is split allowed : blocked by query count (`allowed = dnsQueries − dnsBlocked`), the cache half hit : WAN by bytes, all from `GET /stats/summary?range=15m`, polled every 10 s. "Full in ~N days" on the storage band: `growthPerDay = (cacheBytesStored − evictedBytes) / 7` from `/stats/summary?range=7d`; `days = (freeBytes − minFreeBytes) / growthPerDay` (hidden when growth ≤ 0).

## Tokens

Neutrals are cool and slightly blue-grey, like a server rack in daylight, never warm cream and never pure black.

```
/* light */                      /* dark */
--bg:        #f4f6f8             --bg:        #12161c
--surface:   #ffffff             --surface:   #1a2029
--surface-2: #eef1f4             --surface-2: #222a35
--line:      #d7dde4             --line:      #2e3845
--text:      #17202b             --text:      #e6ebf1
--text-2:    #4d5a69             --text-2:    #a3afbd
--text-3:    #74808e             --text-3:    #7a8695
--focus:     #1f5fbf             --focus:     #8ab8ff
--danger:    #b42318             --danger:    #ff8b7e
--warning:   #9a6700             --warning:   #e3b341
```

Status colours for health (online/degraded/offline) reuse green/warning/danger. Pair colours are only for traffic meaning, never for decoration.

**Type.** One family: **Atkinson Hyperlegible Next** (variable, OFL, self-hosted woff2, latin + latin-ext subset). It was designed by the Braille Institute for maximum character distinction, which matters here because the UI is mostly domain names, IPs and hashes, where `0/O`, `1/l/I` and `rn/m` confusion costs time. **Atkinson Hyperlegible Mono** is used only for machine values: domains, IPs, MACs, paths, hashes and rule text. It is never used for labels or headings. All numbers use `font-variant-numeric: tabular-nums`.

Scale (1.25 ratio, 15 px base, since dense admin UIs read better slightly smaller): 12 / 13 / 15 / 19 / 24 / 30 px. Weights 400, 600 and 700 only. Line height 1.45 for body text, 1.2 for headings. Sentence case everywhere: no all-caps labels, no tracked-out eyebrows.

**Space.** 4 px grid: 4, 8, 12, 16, 24, 32, 48.

**Radius by hierarchy.** Controls 6 px, panels 10 px, chips fully rounded, tables 0 (they sit inside panels). No drop shadows on panels. Separation comes from `--line` borders and surface steps. The only shadow is on floating layers (menus, dialogs): `0 8px 24px rgb(0 0 0 / .18)`.

**Motion.** Only in response to actions: a dialog opening (120 ms), a row expanding, a toast confirming. Live data updates in place without animation, except the pair strip, which eases width changes over 600 ms. Respect `prefers-reduced-motion` (no transitions at all).

## Layout

```
┌──────────────────────────────────────────────────────────────────────┐
│▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓░░░░░▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒████████  ← pair strip       │
├────────────┬─────────────────────────────────────────────────────────┤
│ PiCache    │ Page title                [status: DNS ● Blocking ▾ Cache ●] [search] │
│            ├─────────────────────────────────────────────────────────┤
│ Overview   │                                                         │
│ DNS        │   content: max-width 1440px, left aligned, 24px gutters │
│  Query log │                                                         │
│  Filtering │                                                         │
│  Clients   │                                                         │
│  Local DNS │                                                         │
│ Cache      │                                                         │
│  Downloads │                                                         │
│  Library   │                                                         │
│  Services  │                                                         │
│  Storage   │                                                         │
│ Settings   │                                                         │
│ System     │                                                         │
└────────────┴─────────────────────────────────────────────────────────┘
```

- Sidebar 232 px; it collapses to a top drawer below 900 px. Content is left-aligned. Tables scroll horizontally inside their panel on small screens and never scroll the page sideways.
- **Overview** is not a card grid. It reads as a single "status sentence" row followed by two broad bands, one per product half:
  - Row 1: a plain-language status line, e.g. "DNS is answering 42 queries/min · 18 % blocked · cache served 38 GB today, 91 % from disk". Each number links to the matching filtered page.
  - Band "DNS": the traffic chart (blue/orange stacked), then top blocked domains and top clients side by side, and "Blocked by purpose" as a compact ranked bar list (orange bars, safe search striped blue).
  - Band "Cache": the throughput chart (green hit vs brown WAN), live downloads, and storage (used/free with a "full in ~N days" estimate).
- **Tables** are the core component: 36 px rows (32 px compact), sticky header, right-aligned numbers, mono cells for machine values, status chips in pair colours, row click opens a side panel (not a new page) with details and actions.
- **Forms**: labels above fields; help text below in `--text-2`; validation messages from the API `field` path shown next to the field; destructive actions need an explicit confirm dialog that names the object ("Delete list HaGeZi Multi?").

## Components (web/src/lib/ui)

Button (primary/secondary/ghost/danger, sizes sm/md), IconButton, Input, Select, Toggle, Checkbox, Textarea, Field (label + help + error), Chip (pair/status variants), Badge, Panel (title + actions slot), Table (columns config, sorting, empty state, loading skeleton rows), SidePanel (drawer), Dialog / ConfirmDialog, Tabs, Toast, Tooltip, Stat (value + label + link, used in the status sentence, not as big-number cards), Chart (uPlot wrapper, themed with pair colours, auto light/dark), PairStrip, TimeRangePicker (15m, 1h, 24h, 7d, 30d; independent of retention), Pager (cursor + offset), CopyButton, Bytes/Duration/RelativeTime formatters, EmptyState (tells the user what to do next).

Icons: a small inline SVG set (stroke 1.5 px, 20 px grid) bundled in `web/src/lib/icons.ts`. No icon fonts and no external sprites.

## Writing

- Name things by what users manage: "Blocklists", "Downloads", "Cached games & updates", not "adlists" or "slices".
- Buttons say what happens: "Add blocklist", "Pause blocking for 5 minutes", "Purge from cache". The toast repeats the verb: "Blocklist added".
- Empty states direct: "No downloads yet. Point a client's DNS at PiCache and start a Steam download."
- Errors say what happened and how to fix it, e.g. "The NAS share is not mounted. PiCache is serving downloads uncached. Mount /srv/picache/nas or check the storage settings."
- German and English: all strings live in `web/src/i18n/{en,de}/*.ts`. German uses "du" (informal, typical for home-lab tools) and sentence case.

## Accessibility and quality floor

WCAG 2.2 AA contrast for text; pair colours meet 3:1 against surfaces and are never the only signal (chips also carry text, charts have a legend and tooltips). Visible focus rings (2 px `--focus`, 2 px offset). Everything works by keyboard. Respect `prefers-color-scheme` with a manual override (system / light / dark) stored in `localStorage`. Layout works down to 360 px wide.
