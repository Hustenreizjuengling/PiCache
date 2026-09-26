<!--
  @component
  Conditional forwarders: domains answered by other DNS servers (router,
  company DNS) or by the default upstreams. The most specific enabled
  forwarder wins. Admins can also import forwarders from a list and select
  rows to enable, disable or delete them together.
  Query: ?tab=forwarders&sel=<forwarder id>
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, UNQUALIFIED_DOMAIN, type BatchAction, type Forwarder } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { BulkBar, Button, EmptyState, Panel, Table, toast, Toggle, type Column } from '$lib/ui'
  import { runBatch } from '../shared/batch'
  import ForwarderPanel from './ForwarderPanel.svelte'
  import { domainLabel, forwarderDomains, forwarderTitle, usesDefault } from './forwarders'
  import ImportDialog from './ImportDialog.svelte'

  const forwarders = resource((signal) => api.dns.forwarders.list({ signal }))

  let addOpen = $state(false)
  let importOpen = $state(false)
  let toggling = $state<number[]>([])
  let checked = $state<number[]>([])
  let busy = $state(false)

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(forwarders.data?.find((f) => f.id === selId))

  async function setEnabled(f: Forwarder, enabled: boolean) {
    toggling = [...toggling, f.id]
    const name = forwarderTitle(f)
    try {
      const domains = forwarderDomains(f)
      const saved = await api.dns.forwarders.update(f.id, {
        domain: domains[0],
        domains: [...domains],
        upstreams: [...f.upstreams],
        enabled,
        comment: f.comment,
      })
      forwarders.set((forwarders.data ?? []).map((x) => (x.id === saved.id ? saved : x)))
      toast.success(enabled ? t('dns.forwarders.enabledToast', { domain: name }) : t('dns.forwarders.disabledToast', { domain: name }))
    } catch (e) {
      toast.error(e)
      void forwarders.refresh()
    } finally {
      toggling = toggling.filter((x) => x !== f.id)
    }
  }

  async function batch(action: BatchAction) {
    busy = true
    try {
      const ok = await runBatch({
        action,
        ids: checked,
        what: (n) => tn('dns.forwarders.count', n),
        run: (req) => api.dns.forwarders.batch(req),
      })
      if (!ok) return
      if (action === 'delete') {
        if (selected && checked.includes(selected.id)) router.setQuery({ sel: null })
        checked = []
      }
      void forwarders.refresh()
    } finally {
      busy = false
    }
  }

  const columns: Column<Forwarder>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'domain', label: t('dns.forwarders.domains'), sortable: true, value: (f) => f.domain, cell: domainCell },
    {
      key: 'upstreams',
      label: t('dns.forwarders.upstreams'),
      mono: true,
      truncate: true,
      width: '38%',
      value: (f) => (usesDefault(f) ? t('dns.forwarders.defaultTarget') : f.upstreams.join(', ')),
      cell: targetCell,
    },
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '40%', value: (f) => f.comment },
  ])
</script>

{#snippet enabledCell(f: Forwarder)}
  <Toggle
    bind:checked={() => f.enabled, (on) => setEnabled(f, on)}
    ariaLabel={t('dns.forwarders.enableNamed', { domain: forwarderTitle(f) })}
    disabled={!session.isAdmin || toggling.includes(f.id)}
  />
{/snippet}

{#snippet domainName(d: string)}
  {#if d === UNQUALIFIED_DOMAIN}
    <span class="single">{domainLabel(d)}</span>
  {:else}
    <span class="mono">{d}</span>
  {/if}
{/snippet}

{#snippet domainCell(f: Forwarder)}
  {@const all = forwarderDomains(f)}
  <span class="domains">
    {@render domainName(all[0])}
    {#if all.length > 1}
      <span class="more" title={all.map(domainLabel).join('\n')}>{tn('dns.forwarders.moreDomains', all.length - 1)}</span>
    {/if}
  </span>
{/snippet}

{#snippet targetCell(f: Forwarder)}
  {#if usesDefault(f)}
    <span class="plain">{t('dns.forwarders.defaultTarget')}</span>
  {:else}
    {f.upstreams.join(', ')}
  {/if}
{/snippet}

<Panel flush title={t('dns.forwarders.title')} description={t('dns.forwarders.description')}>
  {#snippet actions()}
    {#if session.isAdmin}
      <Button icon="upload" onclick={() => (importOpen = true)}>{t('dns.forwarders.import')}</Button>
      <Button variant="primary" icon="plus" onclick={() => (addOpen = true)}>
        {t('dns.forwarders.add')}
      </Button>
    {/if}
  {/snippet}
  <Table
    {columns}
    rows={forwarders.data}
    key={(f) => f.id}
    loading={forwarders.loading && !forwarders.loaded}
    error={forwarders.error && !forwarders.data ? errorText(forwarders.error) : undefined}
    onretry={() => forwarders.refresh()}
    onrowclick={(f) => router.setQuery({ sel: f.id })}
    selected={selected?.id}
    caption={t('dns.forwarders.title')}
    selectable={session.isAdmin}
    bind:checked
    checkLabel={(f) => t('dns.forwarders.selectNamed', { domain: forwarderTitle(f) })}
  >
    {#snippet empty()}
      <EmptyState compact icon="link" title={t('dns.forwarders.empty')} text={t('dns.forwarders.emptyText')}>
        {#if session.isAdmin}
          <Button size="sm" variant="primary" icon="plus" onclick={() => (addOpen = true)}>
            {t('dns.forwarders.add')}
          </Button>
        {/if}
      </EmptyState>
    {/snippet}
  </Table>
  {#if session.isAdmin}
    <BulkBar
      count={checked.length}
      {busy}
      onclear={() => (checked = [])}
      actions={[
        { label: t('common.action.enable'), icon: 'play', onselect: () => batch('enable') },
        { label: t('common.action.disable'), icon: 'pause', onselect: () => batch('disable') },
        { label: t('common.action.delete'), icon: 'trash', danger: true, onselect: () => batch('delete') },
      ]}
    />
  {/if}
</Panel>

<ForwarderPanel bind:open={addOpen} onsaved={() => forwarders.refresh()} />
<ForwarderPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  forwarder={selected}
  onsaved={() => forwarders.refresh()}
  ondeleted={() => forwarders.refresh()}
/>
{#if session.isAdmin}
  <ImportDialog bind:open={importOpen} onimported={() => forwarders.refresh()} />
{/if}

<style>
  .domains {
    display: inline-flex;
    align-items: baseline;
    gap: 0 var(--sp-2);
  }
  .mono {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    white-space: nowrap;
  }
  .single {
    color: var(--text-2);
    font-style: italic;
    white-space: nowrap;
  }
  .more {
    color: var(--text-3);
    font-size: var(--fs-xs);
    text-decoration: underline dotted;
    text-underline-offset: 3px;
    white-space: nowrap;
    cursor: help;
  }
  .plain {
    font-family: var(--font);
    color: var(--text-2);
  }
</style>
