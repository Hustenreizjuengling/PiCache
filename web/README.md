# PiCache web UI

Svelte 5 (runes) + Vite 8 + TypeScript 6 single-page app with a hash router,
uPlot for charts and no other runtime dependencies. It builds into
`../internal/webui/dist`, which the Go binary embeds. Design rules:
`docs/DESIGN.md`. REST contract: `docs/API.md`.

```sh
npm ci
npm run dev      # Vite dev server, proxies /api to http://127.0.0.1:8080
npm run check    # svelte-check: must report 0 errors and 0 warnings
npm run build    # production build into ../internal/webui/dist
```

`npm run build` replaces the embedded `../internal/webui/dist`. To check a
production build without touching it, run
`npx vite build --outDir <temporary directory> --emptyOutDir`.

## Layout

```
index.html               no inline script; public/theme-init.js applies the stored theme before paint
src/main.ts              fonts, app.css, API hooks (401 → login, 421 → host-name screen), mount
src/App.svelte           picks setup / login / app; hosts <Toasts /> and <ConfirmHost />
src/app.css              design tokens (light/dark), base styles, utilities, uPlot theme
src/routes.ts            route table (path → lazily loaded page), sidebar sections
src/shell/               pair strip, sidebar/drawer, top bar (status, blocking menu, search, theme, language, account)
src/pages/               Login, Setup, Overview (+ overview/, auth/) and the section pages
src/pages/dns/**         DNS pages: query log, filtering, clients, local DNS, settings
src/pages/cache/**       cache pages: downloads, library, services, storage, settings
src/pages/system/**      system pages: account, tokens, audit log, application log, notifications, logs & privacy, backup (+ scheduled backups), health, updates
src/lib/api/             typed REST client, polling, SSE
src/lib/ui/              components (import from '$lib/ui')
src/lib/*.ts             router, session, app status, settings forms, time ranges, downloads, formatters, icons, theme, errors
src/i18n/                t(), dictionaries en/ and de/ per namespace
```

Aliases: `$lib` = `src/lib`, `$i18n` = `src/i18n`.

## Rules for page work

- **No new dependencies** without a discussion first. Everything a page
  needs is in `lib/`.
- Shared code (`lib/`, `shell/`, `routes.ts`, `app.css`, `App.svelte`,
  `main.ts`, `i18n/index.svelte.ts`, `i18n/*/common.ts`) is used by every
  page. Change it deliberately, in its own commit, and check the other
  pages. Put page-local components in a folder next to the page
  (`pages/dns/querylog/Row.svelte`).
- A page's files are `pages/<section>/**` and `i18n/{en,de}/<section>.ts`.
  Keep the page file names, because `routes.ts` imports them:
  `dns/{QueryLog,Filtering,Clients,LocalDns,DnsSettings}.svelte`,
  `cache/{Downloads,Library,Services,Storage,CacheSettings}.svelte`,
  `system/{Account,Tokens,Audit,AppLog,Notifications,LogsPrivacy,Backup,Health,Updates}.svelte`.
- **Never render HTML from data**: no `{@html}`, no `innerHTML`. No inline
  scripts, no `eval`, no external requests (CSP `script-src 'self'`,
  `connect-src 'self'`). Inline `style=` / `style:` is allowed.
  Markdown from outside (GitHub release notes) goes through
  `pages/system/updates/markdown.ts` + `ReleaseNotes.svelte`, which build
  elements with text content only and link https URLs only.
- The top bar already shows the page title as `<h1>`: pages start with
  content, section headings are `<h2>` (Panel titles).
- Wrap the page in `<div class="page">` (vertical rhythm between panels).
- Tables are the core component: put them in `<Panel flush>`, row click opens
  a `SidePanel` with details and actions (not a new page).
- Read-only principals (`session.isAdmin === false`, API tokens with scope
  `read`) see admin actions disabled; show `t('common.state.readOnly')` once in
  a `Notice` where editing would happen.
- Destructive actions use `confirm()` with a title that names the object
  ("Delete list HaGeZi Multi?"). Successful actions show a toast that repeats
  the verb ("Blocklist added"). Errors: `toast.error(err)` or an inline
  `Notice` with `errorText(err)`; form fields show `fieldError(err, 'name')`.
- Writing: sentence case, buttons say what happens, empty states say what to
  do next. Machine values (domains, IPs, MACs, paths, hashes, rule text) use
  `mono`, never labels. Pair colours only for traffic meaning (blue answered,
  orange blocked, green cache hit, brown WAN); health uses tones ok/warn/fail.
- Keep filters, tabs and the selected row in the URL (`router.setQuery`) so
  views can be linked and reloaded.
- Must work at 360 px width, by keyboard, in light and dark, in English and
  German, with `prefers-reduced-motion`.

## Pages and routing

```ts
import { router, href } from '$lib/router.svelte'
import type { PageProps } from '../../routes'

let { params }: PageProps = $props()   // params['*'] = sub-path below the page ('' if none)

router.path                    // '/dns/queries'
router.param('domain')         // '' when absent
router.list('status')          // ?status=a&status=b or ?status=a,b → ['a', 'b']
router.all('client')           // ?client=a&client=b → ['a', 'b'] (repeated, not split at commas)
router.setQuery({ domain: 'x', cursor: null })      // merge; null/''/false/undefined remove; replaces history
router.setQuery({ selected: id }, { push: true })   // adds a history entry
router.navigate('/dns/clients', { ip: '192.168.1.5' })
href('/dns/queries', { status: ['blocked-list', 'blocked-rule'], range: '1h' })  // '#/dns/queries?status=…&status=…' (arrays repeat the key)
```

**Incoming links** (other pages, the overview and the global search link here;
support these query parameters):

| Target | Parameters |
|---|---|
| `/` (overview) | `range` (15m, 1h, 24h, 7d, 30d, 90d, 180d, 365d) or `from` + `to` (unix seconds, a custom window of at most 400 days) |
| `#/dns/queries` | `range` (15m, 1h, 6h, 24h, 7d; 30d, 90d, 180d, 365d as far as the query log's retention goes) or `from` + `to` (unix seconds, a custom window), `status` (query statuses, repeated or a comma list; the overview uses every `blocked-*`), `domain` (substring, or `"exact"` in double quotes, passed to the API as is), `client` (IP or name; repeated for every address of one device, at most 32, shown as "Device with N addresses"), `qtype`, `upstream`, `rcode` (reply codes, repeated: any of them), `dnssec` (true, false) |
| `#/dns/clients` | `ip` (open/select that client; the global search sends any IPv4/IPv6 here), `tab` (clients, seen, groups), `range` (24h, 7d, 30d; without it the range last chosen in this browser) |
| `#/cache/downloads` | `client` (IP), `active=true`, `from` + `to` (unix seconds: an explicit time window instead of the range; shown as a removable chip) |
| `#/cache/library`, `#/cache/storage`, `#/cache/settings` | – |
| `#/system/notifications` | `channel` (id: open that channel) |
| `#/system/health` | `section` (warnings, host, databases, thresholds, support: scrolls there), `warnings=open` (only unacknowledged entries; the top bar's badge links here) |
| `#/system/logs` | `section` (privacy, recording, retention, clear) |
| `#/system/app-log` | `level` (debug, info, warn, error), `component` |
| `#/system/account`, `#/system/updates`, `#/system/backup` | – |

## API layer (`$lib/api`)

```ts
import { api, resource, streamQueries, isApiError, type DnsRecord } from '$lib/api'
```

`api` has one typed function per endpoint of docs/API.md; every function takes
an optional trailing `{ signal }`. Types mirror the Go JSON (`src/lib/api/types.ts`).

| Group | Functions |
|---|---|
| `api.auth` | `status() setup(b) login({username,password,totp?}) logout() me() changePassword({currentPassword,newPassword,keepTokens?}) sessions() revokeSession(id) totpBegin(currentPassword) totpConfirm(code) totpDisable(password)` |
| `api.tokens` | `list() create({name,scope,expiresInDays?,currentPassword}) remove(id)` |
| `api.system` | `info() health() overview() audit({search,limit,offset}) backupUrl(includeSecrets) restore(blob, password) restart() update() checkUpdate() applyUpdate({version,currentPassword}) host() databases() log({level,component,limit}) setLogLevel({level,component?,minutes}) clearLogLevel() events({unacknowledged,limit,cursor}) ackEvent(id) ackAllEvents() supportBundle({currentPassword,includeClientNames})` (`supportBundle` resolves with `{blob, filename}`: save it with `saveBlob` from `$lib/download`) |
| `api.backups` | `scheduled() run() fileUrl(name) removeFile(name)` (scheduled backups; their settings are the `backups` section of `api.settings`) |
| `api.notifications` | `channels.{list,create,update,remove,test(id)}`, `events()`, `log(limit?)` |
| `api.settings` | `get() put(all) patch(section, partial) defaults()` |
| `api.dns` | `blocking() setBlocking(enabled, pauseSeconds?) lookup(req) stats() cacheIps() router()`, `records.{list,create,update,remove}`, `forwarders.{list,create,update,remove}` |
| `api.upstreams` | `get() test(upstream) flushCache()` |
| `api.clients` | `list() create(c) update(id,c) remove(id) known(within?)` |
| `api.groups` | `list() create(g) update(id,g) remove(id)` (403 for group 1, `DEFAULT_GROUP_ID`) |
| `api.parental` | `services() groups() group(id) update(id, c) setOverride(id, body) clearOverride(id) pause(id, {minutes} \| {until}) resume(id)` (`update` always sends all four members; `pause` replaces a running pause, `resume` also works without one) |
| `api.filter` | `lists.{list,create,update,remove,refresh(id),refreshAll}`, `catalog()`, `rules.{list(query),create,update,remove}`, `stats()`, `explain(domain, clientIp?)` |
| `api.downloadCache` | `services() service(id) setEnabled(id,on) setExtraDomains(id,list) createService(s) updateService(id,s) deleteService(id) source() refreshSource() setLabel(groupKey,label) sni()` |
| `api.cache` | `state() services() groups(q) groupDetail(service,key) objects(q) deleteObject(id) pinObject(id,pinned) deleteGroup(service,key) pinGroup(service,key,pinned) purgeService(service) evict() verify(repair) verifyState() live() active() proxyStats() noSlice() resetNoSlice(host) downloads(q) requests(q) sniEvents(q) evictions(q)` |
| `api.storage` | `capabilities() targets() target(id) create(t) update(id,t) remove(id) test(id) apply(id) init(id,adopt) activate(id) snippets(id) benchmark(id,sizeMiB?) benchmarkState() cancelBenchmark()` |
| `api.logs` | `queries(q)` (cursor page), `exportUrl(format, q)` (a plain download link: ndjson or csv with the filters of `queries`), `clear()` (deletes the query log) |
| `api.stats` | `summary(range) dns(range, step?) cache(range, step?, service?) top(kind, range, limit?, {group?}) services(range) clients(range, {group?}) purposes(range) qtypes(range) clientSeries(key, range, step?) reset()` – `range` is a preset (`'24h'`) or `{ from, to }`; `group: 'device'` (clients, cache-clients) merges the addresses of one device into one row with `addresses`; `clientSeries` keys: an address, `ip:<address>`, `client:<id>`, `mac:<MAC>`; `reset()` deletes the statistics |

Long-running calls (list/source refresh, storage test, starting a storage
speed test, restore) already carry longer timeouts. The backup is a plain link: `<Button href={api.system.backupUrl(true)} download>`; so is a stored scheduled backup (`api.backups.fileUrl(name)`).

**Errors.** Every failure is an `ApiError { status, code, message, field? }`;
`code` is one of `invalid not_found conflict forbidden unavailable unauthorized
too_many_requests internal misdirected` plus client-side `network` and
`aborted`. `isApiError(err, 'conflict')` narrows. A 401 anywhere switches the
app to the login screen (the URL is kept); a 421 shows the host-name screen.

```ts
import { errorText, fieldError } from '$lib/errors'
errorText(err)                        // user-facing sentence (translated for transport errors)
fieldError(err, 'upstreams')          // message if err.field is 'upstreams', 'upstreams[2]' or 'upstreams.x'
```

**Loading and polling.** `resource()` binds a request to the component: it
starts on mount, reloads when reactive values read synchronously in the loader
change, polls with `interval` (paused while the tab is hidden, never
overlapping) and aborts on destroy.

```ts
const range = $derived(router.param('range') || '1h')
const top = resource((signal) => api.stats.top('blocked', range, 10, { signal }), { interval: 30_000 })
// top.data · top.error (ApiError) · top.loading · top.loaded · top.refresh() · top.set(value)

// interval may be a function, asked before every wait: poll fast only while a job runs
const bench: Resource<BenchmarkStatus> = resource((s) => api.storage.benchmarkState({ signal: s }), {
  interval: () => (bench.data?.run?.state === 'running' ? 1_000 : 15_000),
})
```

`new Resource(loader, opts)` + `start()/stop()` for manual control;
`poll(fn, ms)` returns a stop function. App-wide status is already polled every
10 s: `appStatus.overview.data` (`/system/overview`) and `appStatus.strip.data`
(`/stats/summary?range=15m`) from `$lib/status.svelte`; call
`appStatus.overview.refresh()` after changing blocking, the download cache or storage.
`appStatus.update.data` (`/system/update`) is loaded once per page load and
then hourly; it drives the "update available" dot on Updates in the
navigation. The updates page puts its fresher answers into it (`set()`).

**Cursor lists** (query log, cache requests, SNI events, evictions):

```ts
import { CursorStack } from '$lib/ui'
const pages = new CursorStack()
const log = resource((s) => api.logs.queries({ ...filters, cursor: pages.current || undefined }, { signal: s }))
// <Pager mode="cursor" hasPrev={pages.hasPrev} hasNext={!!log.data?.next} count={log.data?.items.length}
//        onprev={() => pages.prev()} onnext={() => pages.next(log.data!.next!)} onfirst={() => pages.reset()} />
// call pages.reset() whenever a filter changes
```

**Live streams (SSE).** Reconnect with backoff (the server ends streams after
1 h), pause while hidden, batched delivery, `state` and `paused` are reactive:

```ts
const live = streamQueries({ client, status }, {
  onEvents: (batch) => (rows = [...batch.reverse(), ...rows].slice(0, 500)),
  onReconnect: () => log.refresh(),   // events may have been missed
})
$effect(() => () => live.close())
// live.pause() · live.resume() · live.state: connecting | open | paused | retrying | closed
```

`streamCache(opts)` streams cache requests. At most 16 streams exist per
server: open one per page, close it on destroy. `streamSystemLog({level,
component}, opts)` streams the application log (admins; at most 4 such
streams, counted separately).

**Settings sections** (`$lib/settings.svelte`): load a section plus its
defaults, edit a draft, save only the changed members (important: the filter
section also holds the blocking pause). While a form has unsaved edits,
leaving the page (links, Back, reload) asks first ("Discard unsaved
changes?"); other editors can register the same guard with
`$effect(() => guardLeave(() => dirty))` from `$lib/router.svelte`.

```ts
const form = settingsForm('dns')
// form.draft (bind to it) · form.saved · form.defaults · form.dirty · form.changes
// await form.save() → boolean · form.revert() · form.resetToDefault('cacheSize') · form.isDefault('cacheSize')
// form.error('upstreams') → field message · form.errorMessage · form.loadError · form.loading · form.saving
```

## Components (`$lib/ui`)

All components are keyboard accessible, themed and translated. Props marked
`bind:` are bindable.

| Component | Props (defaults) | Notes |
|---|---|---|
| `Button` | `variant` primary\|secondary\|ghost\|danger (secondary), `size` sm\|md, `icon`, `loading`, `href`, `download`, `type` (button), `disabled`, + button attributes | One primary per view. With `href` renders a link. |
| `IconButton` | `icon`, `label` (accessible name + tooltip), `variant` ghost\|secondary\|danger, `size`, `loading`, `pressed`, `href` | |
| `Input` | `bind:value`, `type`, `mono`, `invalid`, `icon`, `size`, `bind:ref`, + input attributes | Inside `Field` it gets id/aria automatically. |
| `Select` | `bind:value`, `options: SelectOption[]` ({value,label,disabled?}), `placeholder`, `invalid`, `size` | Native select. |
| `Textarea` | `bind:value`, `rows` (4), `mono`, `invalid` | One entry per line lists. |
| `Toggle` | `bind:checked`, `label`, `description`, `disabled`, `ariaLabel`, `onchange(checked)` | role=switch, for settings that apply immediately. |
| `Checkbox` | `bind:checked`, `bind:indeterminate`, `label`, `description`, `disabled`, `ariaLabel`, `onchange(checked)` | |
| `Field` | `label`, `help`, `error`, `required`, `optional`, `hideLabel`, `id`, children | Label above, error + help below. |
| `Chip` | `pair` blue\|orange\|green\|brown, `striped`, `tone` neutral\|info\|ok\|warn\|fail, `size` sm\|md, `icon`, `label` or children, `title` | Always carries text. |
| `QueryStatusChip` / `CacheStatusChip` / `HealthChip` | `status`, `size` (sm) | Translated labels, fixed colours (`lib/traffic.ts`). |
| `Badge` | `tone`, `title`, children | Counts and tags ("Default", "3"). |
| `Panel` | `title`, `description`, `level` 2\|3, `flush`, `id`, snippets `actions`, `footer`, children | Tables go into `flush` panels. |
| `Table<T>` | `columns: Column<T>[]`, `rows`, `key(row)`, `loading`, `error`, `onretry`, `skeletonRows` (5), `bind:sort` / `onsort` (server-side), `onrowclick`, `selected`, `compact`, `maxHeight` (sticky header), `caption`, `emptyText` / snippet `empty`, `rowClass` | `Column`: `key`, `label`, `align`, `mono` (kept on one line), `wrap` (a long mono value may break), `sortable`, `width`, `value(row)` (sort + default text), `format(row)`, `cell` snippet, `title`, `truncate`. Without `onsort` sorting is client-side by `value`. A `truncate` column needs a `width` (e.g. '30%'), and the short one-line columns next to it '1%', or it shrinks to its header. Empty/error messages render below the table, within the visible width. |
| `Pager` | offset: `total`, `bind:limit` (50), `bind:offset`, `onchange(offset)`; cursor: `mode="cursor"`, `hasPrev`, `hasNext`, `onprev`, `onnext`, `onfirst`, `count`; `limits` + `onlimit` | Place it directly below the table inside the flush Panel (it draws its own top border). |
| `SidePanel` | `bind:open`, `title`, `subtitle`, `size` md\|lg, `dismissible`, `onclose`, snippet `actions`, children | Drawer from the right for row details. |
| `Dialog` | `bind:open`, `title`, `subtitle`, `size` sm\|md\|lg, `dismissible` (true), `onclose`, snippet `actions`, children | Content mounts only while open (forms start fresh). |
| `DurationDialog` | `bind:open`, `title`, `submitLabel`, `maxMinutes` (7 days), children (the explanation above the fields), `onsubmit(minutes)` | Minutes or hours, shows when it ends on the host's clock; an error thrown by `onsubmit` is shown and keeps the dialog open. "Pause … for a custom time". |
| `ConfirmDialog` | `bind:open`, `title`, `message`, `confirmLabel`, `cancelLabel`, `danger` (true), `onconfirm` (async; errors stay in the dialog), `oncancel`, children | Prefer `confirm()`. |
| `confirm(opts)` | `{ title, message?, confirmLabel, cancelLabel?, danger?, action? }` → `Promise<boolean>` | With `action` the dialog runs it with a spinner and shows its error. |
| `toast` | `toast.success(msg)`, `toast.info(msg)`, `toast.error(errOrMsg)` | While a Dialog/SidePanel is open, toasts show in it (above its footer when they would cover it), so they stay clickable. |
| `Tabs` | `tabs: TabItem[]` ({id,label,count?,icon?}), `bind:active`, `label`, `onchange(id)`, snippet `children(active)` | Keep `tab` in the URL. |
| `Menu` | `items: MenuItem[]` ({label, icon?, danger?, disabled?, checked?, href?, download?, onselect?}, {separator:true} or {note}), `label`, `icon`, `iconOnly`, `variant` secondary\|ghost, `size`, `align` start\|end (end), `disabled`, snippet `trigger` | Row "more actions" menus: `iconOnly icon="more"`. `{ note: '…' }` is a short, non-focusable explanation between the items that also describes the menu (e.g. what a pause keeps on). |
| `Tooltip` | `text`, `focusable` (true), children | Never for essential information. |
| `Stat` | `value`, `label`, `href`, `title`, `tone` | Inline linked number (status sentences), not a card. |
| `Chart` | `label`, `timestamps` (unix s), `series: ChartSeries[]` ({label, values, pair?, colorVar?, dashed?, fill?}), `stacked`, `height` (220), `yFormat` (formatCompact), `valueFormat`, `loading`, `minMax` (1), `integer` (true) | uPlot; legend doubles as the tooltip; follows theme and width. |
| `Meter` | `label`, `max`, `segments: MeterSegment[]` ({label, value, text?, pair?, tone?}), `rest` {label,text}, `marker` {value,label}, `legend` (true) | Used/free bars. |
| `PairStrip` | `allowed`, `blocked`, `hit`, `wan`, `caption` | Used by the shell. |
| `TimeRangePicker` | `bind:value` (24h), `options` (15m 1h 24h 7d 30d), `more` (presets under "More"), `custom` ({from,to} shown instead of a preset), `oncustom` (adds "Custom range…" to "More"), `label`, `onchange(range)` | `value={null}`: nothing selected (an explicit from/to window is shown instead). Offer only presets the retention keeps (`withinRetention` from `$lib/range`). |
| `CustomRangeDialog` | `bind:open`, `value` ({from,to} or null), `earliest` (unix s: the retention), `onapply({from,to})` | Local date and time; start before end, at most 400 days, not in the future. |
| `KeyValue` | `items: KeyValueItem[]` ({label, value, mono?, href?}), children (extra `<dt>/<dd>`) | Details in side panels. |
| `Notice` | `tone` info\|ok\|warn\|fail, `title`, `icon`, `ondismiss`, snippet `actions`, children | What happened + how to fix it. |
| `EmptyState` | `title`, `text`, `icon`, `compact`, children (actions) | Says what to do next. |
| `CopyButton` | `text`, `label`, `showLabel`, `size` (sm) | Works on plain-HTTP LAN addresses, also inside dialogs; reports a failure instead of a false "Copied". |
| `Icon` | `name: IconName`, `size` (20), `label` | Decorative unless labelled. |
| `Skeleton` / `Spinner` | `width`, `height` / `size`, `label` | |
| `Trans` | `key`, `params`, one snippet per placeholder | Translations with links or chips inside. |

Icons (`lib/icons.ts`, 20 px grid, 1.5 px stroke): menu close search
chevron-down chevron-up chevron-left chevron-right first check plus minus sun
moon monitor globe logout pause play shield shield-check shield-off alert info
error success copy external refresh trash edit pin download upload filter more
sort arrow-up arrow-down overview list users user home sliders layers grid
drive key lock document terminal archive activity clock eye eye-off link power
update bell send; neutral category icons (never brand logos) video chat gamepad
music sparkles heart dice cart cloud newspaper.

CSS utilities (`app.css`): `.page`, `.stack`, `.stack-sm`, `.row`, `.spacer`,
`.cols-2`, `.toolbar`, `.mono`, `.num`, `.muted`, `.subtle`, `.small`,
`.xsmall`, `.nowrap`, `.truncate`, `.visually-hidden`. Tokens: `--bg`,
`--surface`, `--surface-2`, `--surface-3`, `--line`, `--line-strong`,
`--text`, `--text-2`, `--text-3`, `--focus`, `--danger`, `--warning`, pair
colours `--blue --orange --green --brown`, tones `--ok --warn --fail`, sizes
`--fs-xs … --fs-2xl`, spacing `--sp-1 … --sp-7` (4 px grid), radii
`--r-control --r-panel --r-pill`, `--shadow-float`, `--dur-fast`.

## Formatters and helpers

```ts
import { formatNumber, formatCompact, formatPercent, formatBytes, formatRate, formatDuration,
         formatMicros, formatRelative, formatDateTime, formatDateTimeShort, formatTime, formatDate,
         sameDay } from '$lib/format'
formatNumber(12345)          // "12,345" / "12.345"
formatPercent(0.183)         // ratio 0..1 → "18%" / "18 %"
formatBytes(38e9)            // "38 GB" (decimal units)
formatRate(112e6)            // "112 MB/s"
formatDuration(ms)           // "850 ms", "4.2 sec", "3 hr 5 min"
formatMicros(us)             // DNS durations
formatRelative(iso)          // "5 minutes ago"
formatDateTime(iso, seconds?) · formatTime(iso) · formatDate(iso)
formatDateTimeShort(iso, seconds?)  // dense tables: "Sep 24, 2:05 PM" / "24. Sept., 14:05" (year only if not the current one)
formatSpan(from, to)         // a time window: "Sep 12, 14:00 – Sep 20, 18:00"
sameDay(a, b)                // same local calendar day
```

All formatters follow the active language and return "–" for missing values.

- `lib/traffic.ts`: `queryStatusStyle(status)`, `cacheStatusStyle(status)`, `isBlockedStatus(status)`, `healthTone(status)`; `BLOCKED_STATUSES` and `DEFAULT_GROUP_ID` come from `$lib/api`.
- `lib/session.svelte.ts`: `session.user`, `session.isAdmin`, `session.status` (AuthStatus), `session.logout()`.
- `lib/theme.svelte.ts`: `theme.effective` ('light' | 'dark'), `prefersReducedMotion()`.
- `lib/storage.ts`: `loadPref(key)` / `savePref(key, value)` for per-browser conveniences only (never secrets or server state).
- `lib/range.ts`: `Range` (a preset or `{from, to}` in unix seconds, usable as the API's range), `readRange(presets, fallback)` / `rangeParams(range, fallback)` (the URL's `range` or `from`+`to`), `isCustom`, `isLong` (more than 7 days: daily top tables, load panels one after another), `chartStep` (one point per day from 90 days), `withinRetention(presets, seconds)`, `rangeText`.
- `lib/download.ts`: `saveBlob(blob, name)` and `fileStamp()` for files built or fetched in the page (support bundle, application log).
- `lib/series.ts`: `seriesRates(series, keys, per, asOfMs)` turns a statistics series into rates per `per` seconds for charts. The last bucket of a range that ends now is still filling: it is divided by the time elapsed in it, and left out while that is less than 30 s or a quarter of the step, whichever is longer.

## Translations (`$i18n/index.svelte`)

```ts
import { t, tn } from '$i18n/index.svelte'
t('dns.queryLog.title')
t('dns.lists.deleteTitle', { name: list.name })       // {name} placeholders; numbers are formatted
tn('dns.queryLog.results', count)                     // uses 'queryLog.results.one' / '.other', provides {count}
```

`tn()` picks the form for `count` rounded to one fraction digit, as `{count}`
is shown. If the sentence shows the number rounded differently, pass the
rounded number. A sentence with several counts is composed from plural
fragments rather than one message with several numbers.

- One flat dictionary per namespace and language: `i18n/en/dns.ts` is
  `export default { 'queryLog.title': 'Query log', … }`; keys are prefixed by
  the page (`queryLog.`, `filtering.`, `clients.`, `localDns.`, `settings.`),
  used as `t('dns.queryLog.title')`.
- `i18n/de/dns.ts` is typed `const de: Messages<typeof en> = { … }` so
  svelte-check reports missing or extra keys. German uses "du" and sentence
  case.
- Shared strings live in `common` (read-only for page work): actions
  (`common.action.save|cancel|delete|edit|add|refresh|retry|copy|…`), states
  (`common.state.loading|saving|saved|on|off|enabled|disabled|never|readOnly`),
  labels (`common.label.name|comment|groups|status|time|client|domain|service|type|size|actions|details|created|updated|lastSeen`),
  `common.queryStatus.*`, `common.cacheStatus.*`, `common.health.*`,
  `common.range.long.*`, `common.field.required`.
- Messages with links or components inside: `<Trans key="…">{#snippet link()}<a …>…</a>{/snippet}</Trans>`.
- Server messages (API errors) are English and shown as they are.
