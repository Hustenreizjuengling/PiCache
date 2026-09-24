<!--
  @component
  The health checks the server evaluates every 60 s: status, message, what
  to do and a link to the page where it is fixed. Problems come first.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { Health, HealthCheck, HealthStatus } from '$lib/api'
  import type { ApiError } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatRelative, formatTime } from '$lib/format'
  import type { IconName } from '$lib/icons'
  import { href } from '$lib/router.svelte'
  import { Button, HealthChip, Icon, IconButton, Notice, Panel, Skeleton } from '$lib/ui'
  import { byStatus, CHECKS } from './about'

  interface Props {
    health?: Health
    error?: ApiError
    loading: boolean
    onrefresh: () => void
  }

  let { health, error, loading, onrefresh }: Props = $props()

  const checks = $derived(health ? byStatus(health.checks) : [])
  const failures = $derived(checks.filter((c) => c.status === 'fail').length)
  const warnings = $derived(checks.filter((c) => c.status === 'warn').length)

  const ICON: Record<HealthStatus, IconName> = { ok: 'success', warn: 'alert', fail: 'error' }

  function label(c: HealthCheck): string {
    const info = CHECKS[c.name]
    return info ? t(info.label) : c.name
  }
</script>

<Panel title={t('system.health.checks.title')} flush>
  {#snippet actions()}
    {#if health}
      <span class="small muted" title={formatTime(health.checkedAt, true)}>
        {t('system.health.checks.checkedAt', { time: formatRelative(health.checkedAt) })}
      </span>
    {/if}
    <IconButton icon="refresh" label={t('common.action.refresh')} {loading} onclick={onrefresh} />
  {/snippet}

  {#if error && !health}
    <div class="pad">
      <Notice tone="fail" title={t('system.health.checks.loadError')}>
        {errorText(error)}
        {#snippet actions()}
          <Button size="sm" onclick={onrefresh}>{t('common.action.retry')}</Button>
        {/snippet}
      </Notice>
    </div>
  {:else if !health}
    <div class="pad"><Skeleton height="160px" /></div>
  {:else}
    <p class={['summary', 'pad', failures ? 'fail' : warnings ? 'warn' : 'ok']} role="status">
      <span class="ic"><Icon name={failures ? 'error' : warnings ? 'alert' : 'success'} /></span>
      {#if failures || warnings}
        {tn('system.health.checks.problems', failures + warnings)}
      {:else}
        {t('system.health.checks.allOk')}
      {/if}
    </p>
    <ul class="checks">
      {#each checks as c (c.name)}
        {@const info = CHECKS[c.name]}
        <li class={['check', c.status]}>
          <span class="ic"><Icon name={ICON[c.status]} /></span>
          <div class="body">
            <div class="head">
              <span class="name">{label(c)}</span>
              <HealthChip status={c.status} />
            </div>
            {#if c.message}<p class="small msg">{c.message}</p>{/if}
            {#if c.hint}
              <p class="small muted">{t('system.health.checks.hint', { hint: c.hint })}</p>
            {/if}
            {#if c.status !== 'ok' && info?.path && info.page}
              <a class="small" href={href(info.path)}>{t('system.health.checks.goTo', { page: t(info.page) })}</a>
            {/if}
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</Panel>

<style>
  .pad {
    padding: 0 var(--sp-4) var(--sp-4);
  }
  .summary,
  .check {
    --c: var(--ok);
  }
  .summary {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    font-weight: 600;
    padding-bottom: var(--sp-3);
  }
  .checks {
    margin: 0;
    padding: 0;
    list-style: none;
    border-top: 1px solid var(--line);
  }
  .check {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border-bottom: 1px solid var(--line);
  }
  .check:last-child {
    border-bottom: 0;
  }
  .warn {
    --c: var(--warn);
  }
  .fail {
    --c: var(--fail);
  }
  .ic {
    display: flex;
    color: var(--c);
    margin-top: 1px;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    flex: 1;
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
  }
  .name {
    font-weight: 600;
  }
  .msg {
    overflow-wrap: anywhere;
  }
</style>
