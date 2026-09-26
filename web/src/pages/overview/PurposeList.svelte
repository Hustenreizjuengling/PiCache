<!--
  @component
  "Blocked by purpose" in the DNS band: blocked queries of the range as a
  compact ranked bar list by why they were blocked (the list's category, a
  rule, a service, a schedule, the upstream, …); safe-search answers are
  listed too, in striped blue (answered, but restricted). Counted per hour
  (per UTC day beyond 7 days), so the list is labelled with the start it
  covers where that differs from the range.
-->
<script lang="ts">
  import { t, type MessageKey } from '../../i18n/index.svelte'
  import { api, resource, type Purpose } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import type { Range } from '../../lib/range'
  import BarList from './BarList.svelte'
  import { pollInterval, type Lane } from './lane'
  import { coverage, coverageHint } from './topWindow'

  let { range, lane }: { range: Range; lane: Lane } = $props()

  const data = resource(
    (signal) => {
      const r = range
      return lane.forRange(r, signal, () => api.stats.purposes(r, { signal }))
    },
    { interval: () => pollInterval(range) },
  )

  function label(p: Purpose): string {
    const key = `overview.purpose.${p}` as MessageKey
    const s = t(key)
    return s === key ? p : s
  }

  const items = $derived(
    data.data?.purposes.map((p) => ({
      key: p.purpose,
      label: label(p.purpose),
      count: p.count,
      fill: p.purpose === 'safesearch' ? ('safe' as const) : ('orange' as const),
    })),
  )
</script>

<BarList
  title={t('overview.dns.purposes')}
  note={data.data ? coverage(range, data.data.from) : undefined}
  noteTitle={coverageHint(range)}
  {items}
  loading={!data.loaded}
  error={data.error ? errorText(data.error) : undefined}
  onretry={() => data.refresh()}
  emptyText={t('overview.dns.noPurposes')}
/>
