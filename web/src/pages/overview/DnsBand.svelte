<!--
  @component
  Overview band "DNS": the range's key figures (queries, blocked share,
  average processing time, about how many unique domains, active clients),
  allowed/blocked queries per minute (stacked), blocked queries by purpose,
  query types, then top allowed and blocked domains, top clients (one row
  per device with all its addresses; its link shows the queries of every
  address) and the upstreams with their share and response time. While
  DNS statistics are off the band says so instead; while domains are hidden
  the domain lists do. Ranges longer than 7 days read daily top tables:
  those panels load one after another and poll every 5 minutes.
-->
<script lang="ts">
  import { t, tn } from '../../i18n/index.svelte'
  import { api, resource, type LogsSettings, type TopKind } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import { formatCompact, formatMicros, formatNumber, formatPercent } from '../../lib/format'
  import { chartStep, type Range } from '../../lib/range'
  import { seriesRates } from '../../lib/series'
  import { Button, Chart, Notice, Stat, Trans } from '../../lib/ui'
  import Band from './Band.svelte'
  import { Lane, pollInterval } from './lane'
  import { links } from './links'
  import PurposeList from './PurposeList.svelte'
  import QTypeList from './QTypeList.svelte'
  import TopTable from './TopTable.svelte'
  import { coverage, coverageHint, topWindow } from './topWindow'
  import UpstreamTable from './UpstreamTable.svelte'

  /** `logs` undefined while the settings load (everything is shown then). */
  let { range, logs }: { range: Range; logs?: LogsSettings } = $props()

  const statsOff = $derived(logs?.statsEnabled === false)
  const hidden = $derived(!!logs?.hideDomains)
  const lane = new Lane()
  const every = () => pollInterval(range)

  // asOf: the end of the range, for the rate of the bucket still in progress.
  // Counts come from the count rollups (cheap for any range): not queued.
  const series = resource(
    async (signal) => {
      const r = range
      return statsOff ? undefined : { asOf: Date.now(), s: await api.stats.dns(r, chartStep(r), { signal }) }
    },
    { interval: every },
  )
  const summary = resource(
    (signal) => {
      const r = range
      return statsOff ? Promise.resolve(undefined) : lane.forRange(r, signal, () => api.stats.summary(r, { signal }))
    },
    { interval: every },
  )

  // Top lists are hourly: short ranges get an hour-aligned window, labelled with its start.
  // Clients are grouped by device, so a phone with changing IPv6 addresses is one row.
  function top(kind: TopKind, signal: AbortSignal, skip = false) {
    const w = topWindow(range) // read before the first await: reloads when the range changes
    if (statsOff || skip) return Promise.resolve(undefined)
    const group = kind === 'clients' ? 'device' : undefined
    return lane.forRange(range, signal, () => api.stats.top(kind, w.arg, 10, { signal, group }))
  }
  const domains = resource((signal) => top('domains', signal, hidden), { interval: every })
  const blocked = resource((signal) => top('blocked', signal, hidden), { interval: every })
  const clients = resource((signal) => top('clients', signal), { interval: every })
  const upstreams = resource((signal) => top('upstreams', signal), { interval: every })

  /** The start the top lists cover (a label where it differs from the range). */
  const note = $derived(coverage(range, summary.data?.topFrom))
  const noteTitle = $derived(coverageHint(range))

  const chart = $derived.by(() => {
    if (!series.data) return { timestamps: [] as number[], allowed: [] as number[], blocked: [] as number[] }
    const keys = ['allowed', 'cached', 'override', 'other', 'blocked'] as const
    const { timestamps, values: v } = seriesRates(series.data.s, keys, 60, series.data.asOf)
    return {
      timestamps,
      allowed: timestamps.map((_, i) => v.allowed[i] + v.cached[i] + v.override[i] + v.other[i]),
      blocked: v.blocked,
    }
  })

  const hiddenNote = $derived(hidden ? t('overview.dns.domainsHidden') : undefined)
</script>

<Band title={t('overview.dns.title')} split={statsOff ? undefined : lists}>
  {#snippet actions()}
    <Button size="sm" variant="ghost" icon="list" href={links.queries(range)}>{t('overview.dns.openLog')}</Button>
  {/snippet}

  {#if statsOff}
    <div class="pad">
      <Notice tone="info" title={t('overview.dns.statsOff')}>
        {t('overview.dns.statsOffText')}
        {#snippet actions()}
          <Button size="sm" icon="eye-off" href={links.privacy()}>{t('overview.dns.openPrivacy')}</Button>
        {/snippet}
      </Notice>
    </div>
  {:else}
    {@const s = summary.data}
    <p class="figures pad" aria-live="polite">
      {#if s}
        <span class="fig"><Stat value={formatNumber(s.dnsQueries)} label={tn('overview.fig.queries', s.dnsQueries)} href={links.queries(range)} /></span>
        <span class="fig"><Stat value={formatPercent(s.blockedPercent / 100)} label={t('overview.fig.blocked')} href={links.blocked(range)} /></span>
        <span class="fig"><Stat value={formatMicros(s.avgDnsDurationUs)} label={t('overview.fig.avgTime')} /></span>
        <span class="fig" title={t('overview.fig.uniqueHint')}>
          <Trans key={s.uniqueDomainsEstimated ? 'overview.fig.uniqueAbout' : 'overview.fig.unique'}>
            {#snippet n()}<Stat value={formatNumber(s.uniqueDomains)} label={tn('overview.fig.domains', s.uniqueDomains)} />{/snippet}
          </Trans>
        </span>
        <span class="fig"><Stat value={formatNumber(s.activeClients)} label={tn('overview.fig.clients', s.activeClients)} /></span>
      {:else if summary.error}
        <span class="err">{errorText(summary.error)}</span>
      {:else}
        <span class="subtle">{t('common.state.loading')}</span>
      {/if}
    </p>

    <div class="pad">
      <Chart
        label={t('overview.dns.chartLabel')}
        timestamps={chart.timestamps}
        stacked
        loading={series.loading && !series.loaded}
        series={[
          { label: t('overview.dns.allowed'), pair: 'blue', values: chart.allowed },
          { label: t('overview.dns.blocked'), pair: 'orange', values: chart.blocked },
        ]}
        yFormat={formatCompact}
        valueFormat={(v) => t('overview.unit.perMinute', { n: formatNumber(v, v < 10 ? 1 : 0) })}
      />
      {#if series.error}<p class="err">{errorText(series.error)}</p>{/if}
    </div>

    <PurposeList {range} {lane} />
    <QTypeList {range} {lane} />
  {/if}
</Band>

{#snippet lists()}
  <TopTable
    title={t('overview.dns.topDomains')}
    note={hidden ? undefined : note}
    {noteTitle}
    notice={hiddenNote}
    items={domains.data}
    loading={domains.loading && !domains.loaded}
    error={domains.error && !domains.data ? errorText(domains.error) : undefined}
    onretry={() => domains.refresh()}
    keyLabel={t('overview.dns.domain')}
    countLabel={t('overview.dns.queries')}
    emptyText={t('overview.dns.noDomains')}
    mono
    pair="blue"
    link={(it) => links.domain(it.key, range)}
  />
  <TopTable
    title={t('overview.dns.topBlocked')}
    note={hidden ? undefined : note}
    {noteTitle}
    notice={hiddenNote}
    items={blocked.data}
    loading={blocked.loading && !blocked.loaded}
    error={blocked.error && !blocked.data ? errorText(blocked.error) : undefined}
    onretry={() => blocked.refresh()}
    keyLabel={t('overview.dns.domain')}
    countLabel={t('overview.dns.queries')}
    emptyText={t('overview.dns.noBlocked')}
    mono
    pair="orange"
    link={(it) => links.blockedDomain(it.key, range)}
  />
  <TopTable
    title={t('overview.dns.topClients')}
    {note}
    {noteTitle}
    items={clients.data}
    loading={clients.loading && !clients.loaded}
    error={clients.error && !clients.data ? errorText(clients.error) : undefined}
    onretry={() => clients.refresh()}
    keyLabel={t('overview.dns.client')}
    countLabel={t('overview.dns.queries')}
    emptyText={t('overview.dns.noClients')}
    mono
    link={(it) => links.client(it.addresses?.length ? it.addresses : it.key, range)}
  />
  <UpstreamTable
    {note}
    {noteTitle}
    items={upstreams.data}
    loading={upstreams.loading && !upstreams.loaded}
    error={upstreams.error && !upstreams.data ? errorText(upstreams.error) : undefined}
    onretry={() => upstreams.refresh()}
    link={(it) => links.upstream(it.key, range)}
  />
{/snippet}

<style>
  .pad {
    padding: 0 var(--sp-4);
  }
  .figures {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-5);
    color: var(--text-2);
    min-height: 1.5em;
  }
  .fig {
    white-space: nowrap;
  }
  .err {
    margin-top: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
