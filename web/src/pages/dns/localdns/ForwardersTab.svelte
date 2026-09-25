<!--
  @component
  Conditional forwarders: domains answered by other DNS servers (router,
  company DNS). The most specific enabled forwarder wins.
  Query: ?tab=forwarders&sel=<forwarder id>
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type Forwarder } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, EmptyState, Panel, Table, toast, Toggle, type Column } from '$lib/ui'
  import ForwarderPanel from './ForwarderPanel.svelte'

  const forwarders = resource((signal) => api.dns.forwarders.list({ signal }))

  let addOpen = $state(false)
  let toggling = $state<number[]>([])

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(forwarders.data?.find((f) => f.id === selId))

  async function setEnabled(f: Forwarder, enabled: boolean) {
    toggling = [...toggling, f.id]
    try {
      const saved = await api.dns.forwarders.update(f.id, { domain: f.domain, upstreams: [...f.upstreams], enabled, comment: f.comment })
      forwarders.set((forwarders.data ?? []).map((x) => (x.id === saved.id ? saved : x)))
      toast.success(enabled ? t('dns.forwarders.enabledToast', { domain: f.domain }) : t('dns.forwarders.disabledToast', { domain: f.domain }))
    } catch (e) {
      toast.error(e)
      void forwarders.refresh()
    } finally {
      toggling = toggling.filter((x) => x !== f.id)
    }
  }

  const columns: Column<Forwarder>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'domain', label: t('common.label.domain'), mono: true, sortable: true, value: (f) => f.domain },
    { key: 'upstreams', label: t('dns.forwarders.upstreams'), mono: true, truncate: true, width: '38%', value: (f) => f.upstreams.join(', ') },
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '40%', value: (f) => f.comment },
  ])
</script>

{#snippet enabledCell(f: Forwarder)}
  <Toggle
    bind:checked={() => f.enabled, (on) => setEnabled(f, on)}
    ariaLabel={t('dns.forwarders.enableNamed', { domain: f.domain })}
    disabled={!session.isAdmin || toggling.includes(f.id)}
  />
{/snippet}

<Panel flush title={t('dns.forwarders.title')} description={t('dns.forwarders.description')}>
  {#snippet actions()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
      {t('dns.forwarders.add')}
    </Button>
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
  >
    {#snippet empty()}
      <EmptyState compact icon="link" title={t('dns.forwarders.empty')} text={t('dns.forwarders.emptyText')}>
        <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
          {t('dns.forwarders.add')}
        </Button>
      </EmptyState>
    {/snippet}
  </Table>
</Panel>

<ForwarderPanel bind:open={addOpen} onsaved={() => forwarders.refresh()} />
<ForwarderPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  forwarder={selected}
  onsaved={() => forwarders.refresh()}
  ondeleted={() => forwarders.refresh()}
/>
