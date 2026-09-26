<!--
  @component
  Database sizes (GET /system/databases): picache.db, logs.db with how full
  its size cap is (and why it is not used or raw logging pauses) and the
  index of each cache store; write-ahead logs are shown separately.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, DatabaseInfo } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatPercent } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { Meter, Notice, Panel, Skeleton } from '$lib/ui'

  let { info, error }: { info?: DatabaseInfo; error?: ApiError } = $props()

  function size(bytes: number, wal: number): string {
    return wal > 0 ? t('system.health.db.withWal', { size: formatBytes(bytes), wal: formatBytes(wal) }) : formatBytes(bytes)
  }

  const logs = $derived(info?.logs)
  const fill = $derived(logs && logs.capBytes > 0 ? Math.max(0, Math.min(100, logs.fillPercent)) : 0)
</script>

<Panel id="databases" title={t('system.health.db.title')} description={t('system.health.db.description')}>
  {#if error && !info}
    <Notice tone="fail" title={t('system.health.db.loadError')}>{errorText(error)}</Notice>
  {:else if !info || !logs}
    <Skeleton height="160px" />
  {:else}
    <div class="stack">
      <dl class="dbs">
        <dt>{t('system.health.db.picache')}</dt>
        <dd>{size(info.picache.bytes, info.picache.walBytes)}</dd>

        <dt>{t('system.health.db.logs')}</dt>
        <dd>
          <span>{size(logs.bytes, logs.walBytes)}</span>
          {#if logs.capBytes > 0}
            <span class="cap">
              <Meter
                label={t('system.health.db.cap')}
                max={100}
                segments={[{ label: t('system.health.db.cap'), value: fill, tone: fill >= 90 ? 'warn' : 'info' }]}
                legend={false}
              />
              <span class="small muted">
                {t('system.health.db.capText', { percent: formatPercent(fill / 100), cap: formatBytes(logs.capBytes) })}
                · <a href={href('/system/logs', { section: 'retention' })}>{t('system.health.db.capChange')}</a>
              </span>
            </span>
          {:else}
            <span class="small muted block">{t('system.health.db.noCap')}</span>
          {/if}
        </dd>

        {#each info.cacheIndexes as c (c.storeId)}
          <dt>{t('system.health.db.index', { id: c.storeId })}</dt>
          <dd>{size(c.bytes, c.walBytes)}</dd>
        {/each}
      </dl>

      {#if logs.disabled}
        <Notice tone="fail">{t('system.health.db.disabled', { reason: logs.disabled })}</Notice>
      {:else if logs.rawPaused}
        <Notice tone="warn">{t('system.health.db.rawPaused')}</Notice>
      {/if}
    </div>
  {/if}
</Panel>

<style>
  .dbs {
    display: grid;
    grid-template-columns: minmax(120px, max-content) minmax(0, 1fr);
    gap: var(--sp-2) var(--sp-4);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    color: var(--text-2);
  }
  dd {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
    font-variant-numeric: tabular-nums;
  }
  .cap {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: 6px;
  }
  .block {
    display: block;
  }
  @media (max-width: 480px) {
    .dbs {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    dd {
      margin-bottom: var(--sp-2);
    }
  }
</style>
