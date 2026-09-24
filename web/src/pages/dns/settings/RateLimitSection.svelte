<!--
  @component
  Per-client rate limit (queries per second and burst per /32 or /64),
  exempt networks and the clients that were limited recently, each with an
  "Exempt" action that saves the exemption right away.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type DnsSettings, type DnsStats, type RateLimited } from '$lib/api'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Field, Panel, Table, toast, type Column } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'
  import NumberInput from '../shared/NumberInput.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    stats: DnsStats | undefined
  }

  let { form, stats }: Props = $props()

  const d = $derived(form.draft as DnsSettings)
  let exempting = $state('')

  function isExempt(prefix: string): boolean {
    return !!form.saved?.rateLimitExempt.includes(prefix)
  }

  // Saves only this exemption; other unsaved edits stay in the draft.
  async function exempt(prefix: string) {
    const saved = form.saved
    if (!saved) return
    exempting = prefix
    try {
      const all = await api.settings.patch('dns', { rateLimitExempt: [...new Set([...saved.rateLimitExempt, prefix])] })
      form.saved = all.dns
      if (form.draft && !form.draft.rateLimitExempt.includes(prefix)) {
        form.draft.rateLimitExempt = [...form.draft.rateLimitExempt, prefix]
      }
      toast.success(t('dns.settings.rate.exempted', { client: prefix }))
    } catch (e) {
      toast.error(e)
    } finally {
      exempting = ''
    }
  }

  const columns: Column<RateLimited>[] = $derived([
    { key: 'client', label: t('common.label.client'), mono: true, value: (r) => r.client },
    {
      key: 'dropped',
      label: t('dns.settings.rate.dropped'),
      align: 'right',
      value: (r) => r.dropped,
      format: (r) => formatNumber(r.dropped),
    },
    { key: 'first', label: t('dns.settings.rate.first'), value: (r) => r.first, cell: firstCell },
    { key: 'last', label: t('dns.settings.rate.last'), value: (r) => r.last, cell: lastCell },
    { key: 'actions', label: t('common.label.actions'), align: 'right', width: '1%', cell: actionCell },
  ])
</script>

{#snippet firstCell(r: RateLimited)}
  <span class="nowrap" title={formatDateTime(r.first)}>{formatRelative(r.first)}</span>
{/snippet}

{#snippet lastCell(r: RateLimited)}
  <span class="nowrap" title={formatDateTime(r.last)}>{formatRelative(r.last)}</span>
{/snippet}

{#snippet actionCell(r: RateLimited)}
  {#if isExempt(r.client)}
    <span class="small muted nowrap">{t('dns.settings.rate.isExempt')}</span>
  {:else}
    <Button size="sm" loading={exempting === r.client} disabled={!session.isAdmin || !!exempting} onclick={() => exempt(r.client)}>
      {t('dns.settings.rate.exempt')}
    </Button>
  {/if}
{/snippet}

<Panel id="dns-set-ratelimit" title={t('dns.settings.rate.title')} description={t('dns.settings.rate.description')}>
  <div class="stack">
    <div class="grid">
      <Field label={t('dns.settings.rate.qps')} help={t('dns.settings.rate.qpsHelp')} error={form.error('rateLimitQps')}>
        <NumberInput bind:value={d.rateLimitQps} min={0} max={100000} unit={t('dns.shared.unit.qps')} />
      </Field>
      <Field label={t('dns.settings.rate.burst')} help={t('dns.settings.rate.burstHelp')} error={form.error('rateLimitBurst')}>
        <NumberInput bind:value={d.rateLimitBurst} min={0} max={10000000} unit={t('dns.shared.unit.queries')} />
      </Field>
    </div>
    <Field label={t('dns.settings.rate.exemptList')} optional help={t('dns.settings.rate.exemptHelp')} error={lineError(form.saveError, 'dns.rateLimitExempt')}>
      <LinesInput bind:value={d.rateLimitExempt} rows={3} placeholder="192.168.1.50/32" />
    </Field>

    <section class="stack-sm" aria-labelledby="rate-limited-title">
      <h3 id="rate-limited-title">{t('dns.settings.rate.limitedTitle')}</h3>
      {#if stats}
        <p class="small muted">
          {t('dns.settings.rate.totals', { dropped: formatNumber(stats.rateLimited), overloaded: formatNumber(stats.overloaded) })}
        </p>
      {/if}
      <div class="table">
        <Table
          {columns}
          rows={stats?.topRateLimited ?? []}
          key={(r) => r.client}
          loading={!stats}
          compact
          emptyText={t('dns.settings.rate.none')}
          caption={t('dns.settings.rate.limitedTitle')}
        />
      </div>
    </section>
  </div>
</Panel>

<style>
  h3 {
    font-size: var(--fs-md);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  .table {
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    overflow: hidden;
  }
  @media (max-width: 700px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
