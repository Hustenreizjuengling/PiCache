<!--
  @component
  Where else the log goes (GET /system/log sinks): the log file
  (PICACHE_LOG_FILE) and syslog (PICACHE_LOG_SYSLOG), each with its state,
  the last error and the records it dropped. Both are set on the host and
  receive the same records as the system journal, masked the same way.
  Shown only while a sink is configured.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { LogSink } from '$lib/api'
  import { formatNumber } from '$lib/format'
  import { Chip, Panel } from '$lib/ui'

  let { sinks }: { sinks: readonly LogSink[] } = $props()
</script>

<Panel title={t('system.applog.sinks.title')} description={t('system.applog.sinks.description')}>
  <ul class="sinks">
    {#each sinks as s (s.kind + s.target)}
      <li>
        <div class="head">
          <span class="kind">{t(`system.applog.sinks.${s.kind}`)}</span>
          <span class="mono target">{s.target}</span>
          <Chip size="sm" tone={s.ok ? 'ok' : 'fail'} label={s.ok ? t('system.applog.sinks.ok') : t('system.applog.sinks.failing')} />
        </div>
        {#if s.error}<p class="small err">{s.error}</p>{/if}
        {#if s.dropped > 0}<p class="small muted">{t('system.applog.sinks.dropped', { count: formatNumber(s.dropped) })}</p>{/if}
      </li>
    {/each}
  </ul>
</Panel>

<style>
  .sinks {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  .kind {
    font-weight: 600;
  }
  .target {
    overflow-wrap: anywhere;
  }
  .err {
    color: var(--danger);
    overflow-wrap: anywhere;
  }
</style>
