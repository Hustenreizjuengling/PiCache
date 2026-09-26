<!--
  @component
  "Query types" in the DNS band: the queries of the range by record type
  (GET /stats/qtypes) as a ranked bar list; each type links to its queries.
  Counted per hour (per UTC day beyond 7 days), labelled with the start it
  covers where that differs from the range. Types beyond the 32 counted per
  hour appear as "Other".
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { api, resource } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import type { Range } from '../../lib/range'
  import BarList from './BarList.svelte'
  import { pollInterval, type Lane } from './lane'
  import { links } from './links'
  import { coverage, coverageHint } from './topWindow'

  let { range, lane }: { range: Range; lane: Lane } = $props()

  const data = resource(
    (signal) => {
      const r = range
      return lane.forRange(r, signal, () => api.stats.qtypes(r, { signal }))
    },
    { interval: () => pollInterval(range) },
  )

  const items = $derived(
    data.data?.qtypes.map((q) => ({
      key: q.qtype,
      label: q.qtype === 'OTHER' ? t('overview.dns.otherTypes') : q.qtype,
      count: q.count,
      fill: 'neutral' as const,
      mono: q.qtype !== 'OTHER',
      href: q.qtype === 'OTHER' ? undefined : links.qtype(q.qtype, range),
    })),
  )
</script>

<BarList
  title={t('overview.dns.qtypes')}
  note={data.data ? coverage(range, data.data.from) : undefined}
  noteTitle={coverageHint(range)}
  {items}
  loading={!data.loaded}
  error={data.error ? errorText(data.error) : undefined}
  onretry={() => data.refresh()}
  emptyText={t('overview.dns.noQtypes')}
/>
