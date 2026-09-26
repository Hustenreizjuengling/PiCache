<!--
  @component
  Adds or edits a client: name, identifiers (IP, CIDR or MAC, one per line),
  groups, download cache bypass, "don't log" (raw data) and "don't count"
  (statistics). In edit mode it also shows the client's statistics with the
  addresses they came from (IPv6 addresses the server recognised through the
  device's MAC address included), its activity over the range and links to
  its queries (all those addresses) and downloads.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type Client,
    type ClientGroup,
    type ClientInput,
    type RangePreset,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatBytes, formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { Button, Checkbox, confirm, Field, Input, KeyValue, toast } from '$lib/ui'
  import { isV4 } from '../network/checks'
  import { clientValues } from '../querylog/filters'
  import AddressList from '../shared/AddressList.svelte'
  import ActivityChart from './ActivityChart.svelte'
  import { lineError } from '../shared/errors'
  import { isIP } from '../shared/input'
  import FormPanel from '../shared/FormPanel.svelte'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import LinesInput from '../shared/LinesInput.svelte'
  import type { Totals } from './clientStats'

  interface Props {
    open?: boolean
    /** The client to edit; omit to add one. */
    client?: Client
    /** Initial values for a new client (e.g. from a recently seen address). */
    preset?: Partial<ClientInput>
    groups: readonly ClientGroup[] | undefined
    /** Statistics of the edited client over `range`. */
    totals?: Totals
    range: RangePreset
    onsaved?: (c: Client) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), client, preset, groups, totals, range, onsaved, ondeleted }: Props = $props()

  function blank(): ClientInput {
    return {
      name: '',
      identifiers: [],
      groupIds: [DEFAULT_GROUP_ID],
      comment: '',
      downloadCacheBypass: false,
      ignoreLogs: false,
      ignoreStats: false,
    }
  }

  let draft = $state<ClientInput>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const clientId = $derived(client?.id)
  $effect.pre(() => {
    if (!open) return
    void clientId
    const p = preset
    untrack(() => {
      const c = client
      draft = c
        ? {
            name: c.name,
            identifiers: [...c.identifiers],
            groupIds: [...c.groupIds],
            comment: c.comment,
            downloadCacheBypass: c.downloadCacheBypass,
            ignoreLogs: c.ignoreLogs,
            ignoreStats: c.ignoreStats,
          }
        : { ...blank(), ...p, identifiers: [...(p?.identifiers ?? [])], groupIds: [...(p?.groupIds ?? [DEFAULT_GROUP_ID])] }
      err = undefined
      submitted = false
    })
  })

  const nameError = $derived(fieldError(err, 'name') ?? (submitted && !draft.name.trim() ? t('common.field.required') : undefined))
  const idError = $derived(
    lineError(err, 'identifiers') ?? (submitted && draft.identifiers.length === 0 ? t('dns.clients.identifiersRequired') : undefined),
  )
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)
  /** Room for every identifier (a device's MAC plus its IPv4 and ULA addresses) and one more line. */
  const idRows = $derived(Math.min(8, Math.max(3, draft.identifiers.length + 1)))

  /** Address used for links (queries, downloads): the first IP identifier. */
  const firstIp = $derived(client?.identifiers.find(isIP))
  /** The query log shows the queries of every address the traffic came from. */
  const queriesFor = $derived(totals?.addresses.length ? clientValues(totals.addresses) : (firstIp ?? client?.name ?? ''))
  /** Downloads are filtered by one address (they come over IPv4 almost always). */
  const downloadsFor = $derived(firstIp ?? totals?.addresses.find(isV4) ?? totals?.addresses[0])

  async function save() {
    submitted = true
    err = undefined
    if (!draft.name.trim() || draft.identifiers.length === 0) return
    saving = true
    try {
      const input = { ...draft, name: draft.name.trim(), comment: draft.comment.trim() }
      const saved = client ? await api.clients.update(client.id, input) : await api.clients.create(input)
      toast.success(client ? t('dns.clients.saved', { name: saved.name }) : t('dns.clients.added', { name: saved.name }))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function remove() {
    const c = client
    if (!c) return
    const ok = await confirm({
      title: t('dns.clients.deleteTitle', { name: c.name }),
      message: t('dns.clients.deleteText'),
      confirmLabel: t('dns.clients.delete'),
      action: () => api.clients.remove(c.id),
    })
    if (!ok) return
    toast.success(t('dns.clients.deleted', { name: c.name }))
    open = false
    ondeleted?.(c.id)
  }
</script>

<FormPanel
  bind:open
  title={client ? client.name : t('dns.clients.addTitle')}
  subtitle={client?.identifiers.join(', ')}
  submitLabel={client ? t('common.action.save') : t('dns.clients.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={client ? remove : undefined}
  deleteLabel={t('dns.clients.delete')}
>
  {#snippet header()}
    {#if client && totals}
      <section class="stack-sm" aria-label={t('dns.clients.statsTitle')}>
        <h3>{t('dns.clients.statsTitle')} <span class="muted small">· {t(`common.range.long.${range}`)}</span></h3>
        <KeyValue
          items={[
            { label: t('dns.clients.queries'), value: formatNumber(totals.queries) },
            {
              label: t('dns.clients.blocked'),
              value: totals.queries
                ? `${formatNumber(totals.blocked)} (${formatPercent(totals.blocked / totals.queries)})`
                : formatNumber(totals.blocked),
            },
            { label: t('dns.clients.cacheServed'), value: formatBytes(totals.cacheBytes) },
            {
              label: t('dns.clients.cacheHit'),
              value: totals.cacheBytes ? formatPercent(totals.cacheHitBytes / totals.cacheBytes) : undefined,
            },
            { label: t('common.label.lastSeen'), value: totals.lastSeen ? formatRelative(totals.lastSeen) : t('common.state.never') },
            { label: t('common.label.created'), value: formatDateTime(client.createdAt) },
          ]}
        >
          {#if totals.addresses.length > 0}
            <dt>{t('dns.clients.addresses')}</dt>
            <dd><AddressList addresses={totals.addresses} /></dd>
          {/if}
        </KeyValue>
        <div class="row">
          <Button size="sm" variant="ghost" icon="list" href={href('/dns/queries', { client: queriesFor })}>
            {t('dns.clients.showQueries')}
          </Button>
          {#if downloadsFor}
            <Button size="sm" variant="ghost" icon="download" href={href('/cache/downloads', { client: downloadsFor })}>
              {t('dns.clients.showDownloads')}
            </Button>
          {/if}
        </div>
      </section>
    {/if}
    {#if client}
      <ActivityChart key="client:{client.id}" {range} excluded={client.ignoreStats} />
    {/if}
  {/snippet}

  <Field label={t('common.label.name')} required error={nameError}>
    <Input bind:value={draft.name} maxlength={64} autocomplete="off" />
  </Field>
  <Field label={t('dns.clients.identifiers')} required help={t('dns.clients.identifiersHelp')} error={idError}>
    <LinesInput bind:value={draft.identifiers} rows={idRows} placeholder={'192.168.1.20\naa:bb:cc:dd:ee:ff'} />
  </Field>
  <GroupPicker
    {groups}
    bind:value={draft.groupIds}
    error={fieldError(err, 'groupIds')}
    help={t('dns.clients.groupsHelp')}
    emptyWarning={t('dns.clients.noGroupWarning')}
  />
  <Checkbox bind:checked={draft.downloadCacheBypass} label={t('dns.clients.bypass')} description={t('dns.clients.bypassHelp')} />
  <Checkbox bind:checked={draft.ignoreLogs} label={t('dns.clients.ignoreLogs')} description={t('dns.clients.ignoreLogsHelp')} />
  <Checkbox bind:checked={draft.ignoreStats} label={t('dns.clients.ignoreStats')} description={t('dns.clients.ignoreStatsHelp')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={512} />
  </Field>
</FormPanel>

<style>
  h3 {
    font-size: var(--fs-md);
  }
</style>
