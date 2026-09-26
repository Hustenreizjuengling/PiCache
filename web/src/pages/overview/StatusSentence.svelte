<!--
  @component
  Row 1 of the overview: a plain-language status line whose numbers link to
  the matching filtered pages.
-->
<script lang="ts">
  import { t, tn } from '../../i18n/index.svelte'
  import type { Summary, SystemOverview } from '../../lib/api'
  import { formatBytes, formatDateTimeShort, formatNumber, formatPercent, formatTime, sameDay } from '../../lib/format'
  import { toHost } from '../../lib/hostclock.svelte'
  import { Skeleton, Stat, Trans } from '../../lib/ui'
  import { links } from './links'

  interface Props {
    overview?: SystemOverview
    summary?: Summary
    /** DNS statistics are counted (false: the blocked share is not shown). */
    statsOn?: boolean
  }

  let { overview, summary, statsOn = true }: Props = $props()

  // Rounded as shown (one decimal below 10), so the unit's plural form matches the number.
  const qpm = $derived.by(() => {
    const v = overview ? overview.dns.qps * 60 : 0
    return v < 10 ? Math.round(v * 10) / 10 : Math.round(v)
  })
  const blocking = $derived(overview?.blocking)
  // On the host's clock like the top bar; a pause into another day names the day.
  const pausedText = $derived.by(() => {
    if (!blocking?.pausedUntil) return ''
    const until = toHost(new Date(blocking.pausedUntil))
    return sameDay(until, toHost(new Date())) ? formatTime(until) : formatDateTimeShort(until)
  })
</script>

<p class="sentence">
  {#if !overview}
    <Skeleton width="min(640px, 100%)" height="22px" />
  {:else}
    <span class="clause">
      <Trans key="overview.sentence.dns">
        {#snippet rate()}<Stat
            value={formatNumber(qpm, 1)}
            label={tn('overview.unit.qpm', qpm)}
            href={links.queries()}
          />{/snippet}
      </Trans>
    </span>
    {#if summary && statsOn}
      <span class="sep" aria-hidden="true">·</span>
      <span class="clause">
        <Trans key="overview.sentence.blocked">
          {#snippet share()}<Stat value={formatPercent(summary.blockedPercent / 100)} href={links.blocked()} />{/snippet}
        </Trans>
      </span>
    {/if}
    {#if blocking && !blocking.enabled}
      <span class="sep" aria-hidden="true">·</span>
      <span class="clause warn">
        {#if blocking.pausedUntil && !blocking.permanent}
          {t('overview.sentence.paused', { time: pausedText })}
        {:else}
          {t('overview.sentence.blockingOff')}
        {/if}
      </span>
    {/if}
    <span class="sep" aria-hidden="true">·</span>
    <span class="clause">
      {#if !overview.downloadCacheEnabled}
        <a href={links.cacheSettings()}>{t('overview.sentence.cacheOff')}</a>
      {:else if summary && summary.cacheBytesSent > 0}
        <Trans key="overview.sentence.cache">
          {#snippet bytes()}<Stat value={formatBytes(summary.cacheBytesSent)} href={links.downloads()} />{/snippet}
          {#snippet ratio()}<Stat value={formatPercent(summary.byteHitRatio)} href={links.library()} />{/snippet}
        </Trans>
      {:else}
        <a href={links.downloads()}>{t('overview.sentence.cacheIdle')}</a>
      {/if}
    </span>
  {/if}
</p>

<style>
  .sentence {
    font-size: var(--fs-lg);
    line-height: 1.5;
    color: var(--text-2);
  }
  .clause {
    white-space: normal;
  }
  .sep {
    margin: 0 var(--sp-2);
    color: var(--text-3);
  }
  .warn {
    color: var(--warn);
    font-weight: 600;
  }
  .sentence :global(.stat) {
    color: var(--text);
  }
  a {
    color: var(--text);
  }
</style>
