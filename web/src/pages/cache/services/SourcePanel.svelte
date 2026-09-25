<!--
  @component
  Status of the download service list (uklans/cache-domains): when it was
  fetched, how many services and host names it has, which host patterns were
  rejected, and a button to download it now.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { ApiError, SourceStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, KeyValue, Notice, Panel, Skeleton } from '$lib/ui'

  interface Props {
    source: SourceStatus | undefined
    error: ApiError | undefined
    refreshing: boolean
    onrefresh: () => void
  }

  let { source, error, refreshing, onrefresh }: Props = $props()

  // A successful update starts before it is stored, so a later attempt is one that failed.
  const failedAt = $derived.by(() => {
    const at = source?.lastAttempt
    if (!at) return undefined
    return !source?.lastFetched || Date.parse(at) > Date.parse(source.lastFetched) ? at : undefined
  })
</script>

<Panel title={t('cache.source.title')} description={t('cache.source.description')}>
  {#snippet actions()}
    <Button icon="refresh" loading={refreshing} disabled={!session.isAdmin} onclick={onrefresh}>{t('cache.source.refresh')}</Button>
  {/snippet}

  {#if error && !source}
    <Notice tone="fail">{errorText(error)}</Notice>
  {:else if !source}
    <Skeleton height="96px" />
  {:else}
    <div class="stack">
      <p class="row">
        {#if source.ready}
          <Chip tone="ok" label={t('cache.source.ready')} />
        {:else}
          <Chip tone="warn" label={t('cache.source.notReady')} />
        {/if}
        {#if source.lastFetched}
          <span class="muted small">{t('cache.source.fetched', { time: formatRelative(source.lastFetched) })}</span>
        {/if}
      </p>
      {#if source.error}
        <Notice tone={source.ready ? 'warn' : 'fail'} title={t('cache.source.errorTitle')}>
          {source.error}
          {#if source.ready}<br />{t('cache.source.errorKept')}{/if}
        </Notice>
      {/if}
      <KeyValue
        items={[
          { label: t('cache.source.url'), value: source.source, mono: true },
          { label: t('cache.source.services'), value: formatNumber(source.serviceCount) },
          { label: t('cache.services.hostNames'), value: formatNumber(source.domainCount) },
          { label: t('cache.source.lastFetched'), value: source.lastFetched ? formatDateTime(source.lastFetched) : t('common.state.never') },
          ...(failedAt ? [{ label: t('cache.source.lastFailed'), value: formatDateTime(failedAt) }] : []),
        ]}
      />
      {#if source.skipped && source.skipped.length > 0}
        <details>
          <summary>{tn('cache.source.skipped', source.skipped.length)}</summary>
          <p class="muted small">{t('cache.source.skippedText')}</p>
          <ul class="skipped">
            {#each source.skipped as p, i (i)}
              <li class="mono">{p}</li>
            {/each}
          </ul>
        </details>
      {/if}
      <p class="small"><a href={href('/cache/settings')}>{t('cache.source.settingsLink')}</a></p>
    </div>
  {/if}
</Panel>

<style>
  details {
    font-size: var(--fs-sm);
  }
  summary {
    cursor: pointer;
    font-weight: 600;
  }
  summary:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  details > p {
    margin: var(--sp-2) 0;
  }
  .skipped {
    margin: 0;
    padding: var(--sp-2) var(--sp-3);
    list-style: none;
    max-height: 200px;
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .skipped li {
    overflow-wrap: anywhere;
  }
</style>
