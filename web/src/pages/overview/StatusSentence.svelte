<!--
  @component
  Row 1 of the overview: a plain-language status line whose numbers link to
  the matching filtered pages.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import type { Summary, SystemOverview } from '../../lib/api'
  import { formatBytes, formatNumber, formatPercent, formatTime } from '../../lib/format'
  import { Skeleton, Stat, Trans } from '../../lib/ui'
  import { links } from './links'

  let { overview, summary }: { overview?: SystemOverview; summary?: Summary } = $props()

  const qpm = $derived(overview ? overview.dns.qps * 60 : 0)
  const blocking = $derived(overview?.blocking)
</script>

<p class="sentence">
  {#if !overview}
    <Skeleton width="min(640px, 100%)" height="22px" />
  {:else}
    <span class="clause">
      <Trans key="overview.sentence.dns">
        {#snippet rate()}<Stat
            value={formatNumber(qpm, qpm < 10 ? 1 : 0)}
            label={t('overview.unit.qpm')}
            href={links.queries()}
          />{/snippet}
      </Trans>
    </span>
    {#if summary}
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
          {t('overview.sentence.paused', { time: formatTime(blocking.pausedUntil) })}
        {:else}
          {t('overview.sentence.blockingOff')}
        {/if}
      </span>
    {/if}
    <span class="sep" aria-hidden="true">·</span>
    <span class="clause">
      {#if !overview.lancacheEnabled}
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
