<!--
  @component
  Health warnings from /system/overview; the failing checks' messages are
  loaded from /system/health only when something needs attention.
-->
<script lang="ts">
  import { t, tn } from '../../i18n/index.svelte'
  import { api, resource, type SystemOverview } from '../../lib/api'
  import { Button, Notice } from '../../lib/ui'
  import { links } from './links'

  let { overview }: { overview?: SystemOverview } = $props()

  const MAX_SHOWN = 3

  const problems = $derived(overview ? overview.health.warnings + overview.health.failures : 0)
  const health = resource(
    async (signal) => (problems > 0 ? api.system.health({ signal }) : undefined),
    { interval: 60_000 },
  )
  const checks = $derived((health.data?.checks ?? []).filter((c) => c.status !== 'ok'))
</script>

{#if overview && problems > 0}
  <Notice tone={overview.health.failures > 0 ? 'fail' : 'warn'} title={tn('overview.health.title', problems)}>
    {#if checks.length > 0}
      <ul>
        {#each checks.slice(0, MAX_SHOWN) as c (c.name)}
          <li>{c.message ?? c.name}{#if c.hint}<span class="hint">{` – ${c.hint}`}</span>{/if}</li>
        {/each}
      </ul>
      {#if checks.length > MAX_SHOWN}
        <p>{tn('overview.health.more', checks.length - MAX_SHOWN)}</p>
      {/if}
    {/if}
    {#snippet actions()}
      <Button size="sm" href={links.health()} icon="activity">{t('overview.health.open')}</Button>
    {/snippet}
  </Notice>
{/if}

<style>
  .hint {
    color: var(--text-3);
  }
</style>
