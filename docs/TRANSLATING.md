# Translating PiCache

The web UI speaks English (`en`) and German (`de`). English is the source
and the fallback. **Every text exists in every language**: a change that
adds or changes a UI text translates it into every language (the types and
`npm run check` refuse anything else).

Server texts stay English: API errors, health messages, notifications, the
log and the command line. The list catalogue shows its German description
for German and the English one for every other language.

## Where the texts are

```
web/src/i18n/
  locales.ts            the list of languages (id, name, default tag), one entry per line
  index.svelte.ts       loading, choice of the language, t() and tn()
  types.ts              Messages<T> and Translation<T>
  en/<namespace>.ts     English: common, auth, overview, dns, cache, system
  <id>/<namespace>.ts   the same keys in language <id>
  <id>/<id>.ts          the namespaces of language <id> (one chunk per language)
```

Each namespace file is a flat dictionary `'key': 'value'` with single-quoted
values. English is part of the main bundle; every other language is loaded
when it is chosen (its own file `assets/<id>-<hash>.js`), before the page
switches, so there is no flash of English. The language is the one chosen
in this browser, else the one of **System → Users & security → Web
settings** (`web.language`), else the browser's languages, else English.

## Adding a language

1. Copy `web/src/i18n/en/` to `web/src/i18n/<id>/`, rename `en.ts` to
   `<id>.ts` and translate every value. Type each namespace as
   `Translation<typeof en>` (see `web/src/i18n/de/`).
2. Add the language to `web/src/i18n/locales.ts` (`{ id, label, tag }`, the
   label in the language itself, the tag a BCP 47 tag such as `fr-FR`) and to
   the loader map in `web/src/i18n/index.svelte.ts`.
3. Add the same id at the same place to `settings.Languages` in
   `internal/settings/web.go` (a Go test compares both lists; the API
   accepts only these ids for `web.language`) and to the `web.language`
   enum in `internal/api/openapi.json`.
4. Run `npm run check` and `npm run build` in `web/` (below), and
   `go test ./internal/settings/ ./internal/api/`.
5. Add the address form and the glossary column below.

## The checks

`npm run check` runs svelte-check and then:

- `web/scripts/check-i18n.mjs` (no dependencies) reads every dictionary and
  lists every problem with language, namespace, key and rule:
  1. every language has every namespace file and exactly the English keys,
     plus only the extra plural forms its plural rules need; no value is
     empty;
  2. **plural forms**: for every plural key (`<base>.one`/`<base>.other` in
     English) a language has every category of
     `new Intl.PluralRules(<tag>).resolvedOptions().pluralCategories`
     (Polish and Czech, for example: `one`, `few`, `many`, `other`); in
     languages added after English and German every plural form contains
     `{count}` (French uses `one` for 0 too, Danish for fractions, so "1"
     must not be written out);
  3. **placeholders**: a text has exactly the placeholders of the English
     text (`{name}`, and the slots of rich texts); the forms of a plural key
     have, besides `{count}`, exactly the placeholders of the English forms;
  4. **code-like tokens** are identical to the English text: web addresses,
     absolute paths, `--options`, `PICACHE_*` variables, unit names
     (`*.service`, `*.path`, …), text in back-quotes and the words `sudo`,
     `systemctl`, `journalctl`, `docker`, `picache`, `install.sh`,
     `get-picache.sh`, `apt`, `dnf`, `zypper`, `pacman`, `curl`, `openssl`,
     `nft`, `iptables`, `ufw`, `firewall-cmd` (UI labels in quotes are not
     tokens: they are translated);
  5. the numbers of `<` and `>` equal the English text's;
  6. no `{@html` anywhere in the Svelte files.
- `web/scripts/check-search-index.mjs`: the settings search index points
  at existing routes, anchors and keys.

`npm run build` also checks the bundle sizes (`web/scripts/check-bundle.mjs`):
the main chunk at most 880 000 bytes, each language chunk at most 450 000.

The checks are the floor, not the review: **a maintainer reviews the
code-like parts of every translation pull request** (commands, paths,
options), because a wrong command in a translated text is copied by users.

## Rules

- **Address form**: German *du*.
- **Sentence case** everywhere (buttons, titles, menu entries), as in
  English.
- **Numbers, dates, sizes and units** only through the formatters
  (`Intl`, the helpers of `web/src/lib/format.ts`), never written into a
  text.
- UI labels quoted in a text use `"…"` as in English (the check does not
  treat quoted text as code).
- **Never translate** commands, options, paths, environment variables, unit
  names, setting member names (`dns.upstreams`), and product and protocol
  names: PiCache, DNS, DoT, DoH, DNSSEC, DHCP, DHCPv6, ClientID, IPv4, IPv6,
  ULA, NTP, API, HTTP, HTTPS, TLS, SNI, CIDR, MAC, VPN, Docker, systemd.

## Glossary

The recurring terms of the UI and their fixed rendering per language:

| English | de |
|---|---|
| blocklist | Blockliste |
| allowlist | Erlaubnisliste |
| upstream (DNS server) | Upstream |
| query log | Abfrageprotokoll |
| client | Client |
| group | Gruppe |
| download cache | Download-Cache |
| rule | Regel |
| parental controls | Jugendschutz |
| local record | lokaler Eintrag |
| storage target | Speicherziel |
| health check | Zustandsprüfung |

These are the terms the UI uses today; a new language adds a column, and
when a translation deviates from this table, fix one of them, not both
differently.
