<!--
  @component
  "Blocked by purpose" in the DNS band: blocked queries of the range as a
  compact ranked bar list by why they were blocked (the list's category, a
  rule, a service, a schedule, the upstream, …); safe-search answers are
  listed too, in striped blue (answered, but restricted). Counted per hour,
  so short ranges are labelled with the hour they start in.
-->
<script lang="ts">
  import { t, type MessageKey } from '../../i18n/index.svelte'
  import { api, resource, type Purpose, type RangePreset } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import { formatNumber, formatTime } from '../../lib/format'
  import { Button, Skeleton } from '../../lib/ui'
  import { topWindow } from './topWindow'

  let { range }: { range: RangePreset } = $props()

  const auto = $props.id()
  const data = resource((signal) => api.stats.purposes(range, { signal }), { interval: 60_000 })

  const items = $derived((data.data?.purposes ?? []).filter((p) => p.count > 0))
  const max = $derived(Math.max(1, ...items.map((p) => p.count)))
  const since = $derived(
    data.data && topWindow(range).since !== undefined ? t('overview.dns.topSince', { time: formatTime(data.data.from) }) : undefined,
  )

  function label(p: Purpose): string {
    const key = `overview.purpose.${p}` as MessageKey
    const s = t(key)
    return s === key ? p : s
  }
</script>

<section class="purposes" aria-labelledby="purposes-{auto}">
  <h3 id="purposes-{auto}">
    {t('overview.dns.purposes')}{#if since}<span class="note" title={t('overview.dns.topSinceHint')}>· {since}</span>{/if}
  </h3>
  {#if data.error && !data.data}
    <p class="err">
      {errorText(data.error)}
      <Button size="sm" variant="ghost" icon="refresh" onclick={() => data.refresh()}>{t('common.action.retry')}</Button>
    </p>
  {:else if !data.loaded}
    <Skeleton height="72px" />
  {:else if items.length === 0}
    <p class="small muted">{t('overview.dns.noPurposes')}</p>
  {:else}
    <ol class="bars">
      {#each items as p (p.purpose)}
        <li>
          <span class="name">{label(p.purpose)}</span>
          <span class="bar" aria-hidden="true">
            <span class={['fill', p.purpose === 'safesearch' && 'safe']} style:width="{(p.count / max) * 100}%"></span>
          </span>
          <span class="n">{formatNumber(p.count)}</span>
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  .purposes {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: 0 var(--sp-4);
    min-width: 0;
  }
  h3 {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .note {
    margin-left: var(--sp-2);
    color: var(--text-3);
    font-weight: 400;
  }
  /* Ranked top to bottom, then into the next column. */
  .bars {
    columns: 3 260px;
    column-gap: var(--sp-6);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(40px, 72px) 6ch;
    align-items: center;
    gap: var(--sp-2);
    padding: 3px 0;
    break-inside: avoid;
    font-size: var(--fs-sm);
    line-height: 1.3;
  }
  .name {
    overflow-wrap: anywhere;
  }
  .bar {
    display: flex;
    height: 6px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
    overflow: hidden;
  }
  .fill {
    height: 100%;
    border-radius: var(--r-pill);
    background: var(--orange);
  }
  /* Answered, but restricted: the striped blue of a secondary "answered" state. */
  .fill.safe {
    background: repeating-linear-gradient(-45deg, var(--blue) 0 3px, color-mix(in srgb, var(--blue) 30%, var(--surface)) 3px 6px);
    box-shadow: inset 0 0 0 1px var(--blue);
  }
  .n {
    font-variant-numeric: tabular-nums;
    text-align: right;
  }
  .err {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
