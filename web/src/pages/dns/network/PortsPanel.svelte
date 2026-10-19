<!--
  @component
  Open ports (clients.portChecks, off by default): the settings of the
  regular port checks of the devices of the neighbour table (on/off,
  interval, extra ports), the state of the last and next check and "Check
  now" (admins); below, a panel with the recorded changes, newest first.
  The open ports per device are shown in the devices table above.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, isApiError, resource, toApiError, type ClientsSettings, type NetworkDevice, type PortChange, type PortChecksState } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Chip, EmptyState, Field, Input, Notice, Pager, Panel, Skeleton, Spinner, Table, toast, Toggle, type Column } from '$lib/ui'
  import NumberInput from '../shared/NumberInput.svelte'
  import { rangeError, RANGES } from '../../system/forms'

  interface Props {
    checks: PortChecksState | undefined
    error: unknown
    /** The devices of the network check (names for the changes). */
    devices: readonly NetworkDevice[]
    onchanged: () => void
  }

  let { checks, error, devices, onchanged }: Props = $props()

  const form = settingsForm('clients')
  const d = $derived((form.draft as ClientsSettings | undefined)?.portChecks)

  // ---- extra ports: edited as text, stored as numbers

  let extraText = $state<string | undefined>(undefined)
  const shownExtra = $derived(extraText ?? (d?.extraPorts ?? []).join(', '))
  const parsedExtra = $derived.by(() => {
    const parts = shownExtra.split(/[\s,;]+/).filter(Boolean)
    const out: number[] = []
    for (const p of parts) {
      const n = Number(p)
      if (!/^\d+$/.test(p) || n < 1 || n > 65535) return undefined
      if (!out.includes(n)) out.push(n)
    }
    return out.sort((a, b) => a - b)
  })
  const extraError = $derived(
    parsedExtra === undefined ? t('dns.network.ports.extraInvalid') : parsedExtra.length > 32 ? t('dns.network.ports.extraTooMany') : undefined,
  )
  const intervalError = $derived(d ? rangeError(d.intervalHours, RANGES.portCheckHours) : undefined)
  const invalid = $derived(!!extraError || !!intervalError)

  function setExtra(v: string) {
    extraText = v
    if (d && parsedExtra) d.extraPorts = parsedExtra
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid || !d) return
    if (parsedExtra) d.extraPorts = parsedExtra
    if (await form.save()) {
      extraText = undefined
      toast.success(t('common.state.saved'))
      onchanged()
    }
  }

  // ---- check now

  let starting = $state(false)

  async function runNow() {
    starting = true
    try {
      await api.network.runPorts()
      toast.success(t('dns.network.ports.started'))
    } catch (err) {
      const e = toApiError(err)
      if (isApiError(e, 'conflict')) toast.info(t('dns.network.ports.busy'))
      else if (isApiError(e, 'too_many_requests')) toast.info(t('dns.network.ports.tooSoon'))
      else toast.error(e)
    } finally {
      starting = false
      onchanged()
    }
  }

  // ---- changes

  const LIMIT = 25
  let offset = $state(0)
  const changes = resource((signal) => api.network.portChanges({ limit: LIMIT, offset }, { signal }), { interval: 60_000 })
  // A finished check may have recorded changes.
  $effect(() => {
    void checks?.lastRun
    void changes.refresh()
  })

  const names = $derived.by(() => {
    const byMac = new Map<string, string>()
    for (const dev of devices) if (dev.name) byMac.set(`mac:${dev.mac}`, dev.name)
    return byMac
  })

  const columns = $derived<Column<PortChange>[]>([
    { key: 'time', label: t('dns.network.ports.col.time'), cell: timeCell },
    { key: 'device', label: t('dns.network.ports.col.device'), cell: deviceCell },
    { key: 'port', label: t('dns.network.ports.col.port'), cell: portCell },
    { key: 'change', label: t('dns.network.ports.col.change'), cell: changeCell },
  ])

  const formId = $props.id()
</script>

{#snippet timeCell(c: PortChange)}
  <span class="nowrap" title={formatDateTime(c.time)}>{formatRelative(c.time)}</span>
{/snippet}

{#snippet deviceCell(c: PortChange)}
  {@const name = names.get(c.device)}
  {#if name}{name} <span class="mono subtle">{c.ip}</span>{:else}<span class="mono">{c.ip}</span>{/if}
{/snippet}

{#snippet portCell(c: PortChange)}
  <span class="mono">{c.port}</span>{#if c.service}<span class="subtle svc">{c.service}</span>{/if}
{/snippet}

{#snippet changeCell(c: PortChange)}
  <Chip size="sm" tone={c.opened ? 'warn' : 'neutral'} label={c.opened ? t('dns.network.ports.opened') : t('dns.network.ports.closed')} />
{/snippet}

{#snippet footer()}
  <div class="row">
    <Button type="submit" form="ports-{formId}" variant="primary" loading={form.saving} disabled={!form.dirty || invalid}>
      {t('common.action.save')}
    </Button>
    <Button
      variant="ghost"
      disabled={!form.dirty || form.saving}
      onclick={() => {
        extraText = undefined
        form.revert()
      }}>{t('system.form.discard')}</Button
    >
  </div>
{/snippet}

<Panel id="network-ports" title={t('dns.network.ports.title')} description={t('dns.network.ports.description')} footer={d && session.isAdmin ? footer : undefined}>
  {#snippet actions()}
    {#if session.isAdmin && checks?.enabled}
      <Button icon="refresh" loading={starting} disabled={checks.running} onclick={runNow}>{t('dns.network.ports.run')}</Button>
    {/if}
  {/snippet}

  <div class="stack">
    {#if error && !checks}
      <Notice tone="warn" title={t('dns.network.ports.unavailableTitle')}>{errorText(error)}</Notice>
    {/if}

    {#if form.loadError && !d}
      <Notice tone="fail">{errorText(form.loadError)}</Notice>
    {:else if !d}
      <Skeleton height="120px" />
    {:else}
      <form id="ports-{formId}" onsubmit={save} novalidate>
        <fieldset class="stack" disabled={!session.isAdmin}>
          {#if form.saveError && !form.saveError.field}<Notice tone="fail">{form.errorMessage}</Notice>{/if}
          <Toggle bind:checked={d.enabled} id="ports-field-enabled" label={t('dns.network.ports.enabled')} description={t('dns.network.ports.enabledHelp')} />
          {#if d.enabled}
            <div class="fields">
              <Field id="ports-field-interval" label={t('dns.network.ports.interval')} help={t('dns.network.ports.intervalHelp')} error={form.error('portChecks.intervalHours') ?? intervalError}>
                <NumberInput bind:value={d.intervalHours} min={RANGES.portCheckHours.min} max={RANGES.portCheckHours.max} unit={t('dns.network.ports.hours')} />
              </Field>
              <Field id="ports-field-extra" label={t('dns.network.ports.extra')} help={t('dns.network.ports.extraHelp')} error={form.error('portChecks.extraPorts') ?? extraError}>
                <Input value={shownExtra} oninput={(e) => setExtra(e.currentTarget.value)} placeholder="25565, 8096" />
              </Field>
            </div>
          {/if}
        </fieldset>
      </form>
    {/if}

    {#if checks?.enabled}
      <p class="status small">
        {#if checks.running}
          <Spinner size={14} /> {t('dns.network.ports.running')}
        {:else if checks.lastRun}
          <span title={formatDateTime(checks.lastRun)}>{t('dns.network.ports.lastRun', { when: formatRelative(checks.lastRun) })}</span>
        {:else}
          <span>{t('dns.network.ports.never')}</span>
        {/if}
        {#if checks.nextRun && !checks.running}
          <span class="muted" title={formatDateTime(checks.nextRun)}>· {t('dns.network.ports.nextRun', { when: formatRelative(checks.nextRun) })}</span>
        {/if}
      </p>
      {#if checks.lastError}
        <Notice tone="warn" title={t('dns.network.ports.lastError')}>{checks.lastError}</Notice>
      {/if}
    {/if}

  </div>
</Panel>

{#if checks && (checks.enabled || (changes.data?.total ?? 0) > 0)}
  <Panel id="network-port-changes" flush title={t('dns.network.ports.history')} description={t('dns.network.ports.historyText')}>
    {#if changes.data && changes.data.total === 0}
      <EmptyState compact icon="clock" title={t('dns.network.ports.historyEmpty')} text={t('dns.network.ports.historyEmptyText')} />
    {:else}
      <Table
        caption={t('dns.network.ports.history')}
        rows={changes.data?.items}
        key={(c) => String(c.id)}
        {columns}
        loading={changes.loading && !changes.loaded}
        error={changes.error ? errorText(changes.error) : undefined}
        onretry={() => changes.refresh()}
      />
      {#if (changes.data?.total ?? 0) > LIMIT}
        <Pager total={changes.data?.total ?? 0} limit={LIMIT} {offset} onchange={(o) => ((offset = o), void changes.refresh())} />
      {/if}
    {/if}
  </Panel>
{/if}

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: var(--sp-4);
  }
  .status {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    margin: 0;
  }
  .svc {
    margin-left: 4px;
  }
</style>
