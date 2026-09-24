<!--
  @component
  The network listeners (bootstrap configuration, restart required to
  change): bound addresses, bind failures and listeners that are off.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ListenerInfo } from '$lib/api'
  import { Chip, Panel, Table, type Column } from '$lib/ui'
  import { LISTENER_ROLES } from './about'

  let { listeners }: { listeners?: ListenerInfo } = $props()

  interface Row {
    role: string
    label: string
    env: string
    addresses: string[]
    error?: string
  }

  const rows = $derived.by((): Row[] | undefined => {
    if (!listeners) return undefined
    const bound = listeners.bound ?? {}
    const failed = listeners.failed ?? {}
    const known = new Set(LISTENER_ROLES.map((r) => r.role))
    const extra = [...Object.keys(bound), ...Object.keys(failed)]
      .filter((r, i, all) => !known.has(r) && all.indexOf(r) === i)
      .map((role) => ({ role, label: role, env: '' }))
    return [...LISTENER_ROLES.map((r) => ({ role: r.role, label: t(r.label), env: r.env })), ...extra].map((r) => ({
      ...r,
      addresses: bound[r.role] ?? [],
      error: failed[r.role],
    }))
  })

  const columns = $derived<Column<Row>[]>([
    { key: 'label', label: t('system.health.listeners.role'), value: (r) => r.label },
    { key: 'status', label: t('common.label.status'), cell: statusCell },
    { key: 'addresses', label: t('system.health.listeners.addresses'), cell: addressCell },
    { key: 'env', label: t('system.health.listeners.setting'), mono: true, value: (r) => r.env },
  ])
</script>

{#snippet statusCell(r: Row)}
  {#if r.error}
    <Chip tone="fail" size="sm" label={t('system.health.listeners.failed')} />
  {:else if r.addresses.length > 0}
    <Chip tone="ok" size="sm" label={t('system.health.listeners.listening')} />
  {:else}
    <Chip tone="neutral" size="sm" label={t('system.health.listeners.off')} />
  {/if}
{/snippet}

{#snippet addressCell(r: Row)}
  {#if r.error}
    <span class="err small">{r.error}</span>
  {:else if r.addresses.length > 0}
    <span class="mono">{r.addresses.join(', ')}</span>
  {:else}
    <span class="subtle">–</span>
  {/if}
{/snippet}

<Panel title={t('system.health.listeners.title')} description={t('system.health.listeners.description')} flush>
  <Table {columns} {rows} key={(r) => r.role} loading={!rows} caption={t('system.health.listeners.title')} compact skeletonRows={6} />
</Panel>

<style>
  .err {
    color: var(--danger);
    overflow-wrap: anywhere;
  }
</style>
