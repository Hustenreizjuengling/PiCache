<!--
  @component
  "DNSSEC" in the DNS band: the queries of the range PiCache validated, by
  status (secure, insecure, bogus, indeterminate; GET /stats/dnssec) as a
  ranked bar list with each status's share; each status links to its
  queries. Counted per hour (per UTC day beyond 7 days), labelled with the
  start it covers where that differs from the range. Shown only when the
  range has counts (nothing is validated outside the DNSSEC mode Validate).
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { api, resource, type DnssecStatus } from '../../lib/api'
  import type { Range } from '../../lib/range'
  import { dnssecStatusLabel } from '../dns/shared/dnssec'
  import BarList, { type BarItem } from './BarList.svelte'
  import { pollInterval, type Lane } from './lane'
  import { links } from './links'
  import { coverage, coverageHint } from './topWindow'

  let { range, lane }: { range: Range; lane: Lane } = $props()

  const data = resource(
    (signal) => {
      const r = range
      return lane.forRange(r, signal, () => api.stats.dnssec(r, { signal }))
    },
    { interval: () => pollInterval(range) },
  )

  const FILL: Record<DnssecStatus, BarItem['fill']> = { secure: 'ok', insecure: 'neutral', bogus: 'fail', indeterminate: 'warn' }

  const items = $derived(
    (data.data?.statuses ?? [])
      .filter((s) => s.count > 0)
      .map((s) => ({
        key: s.status,
        label: dnssecStatusLabel(s.status),
        count: s.count,
        fill: FILL[s.status] ?? 'neutral',
        href: links.dnssec(s.status, range),
      })),
  )
</script>

{#if items.length > 0}
  <BarList
    title={t('overview.dnssec.title')}
    note={data.data ? coverage(range, data.data.from) : undefined}
    noteTitle={coverageHint(range)}
    {items}
    shares
    emptyText=""
  />
{/if}
