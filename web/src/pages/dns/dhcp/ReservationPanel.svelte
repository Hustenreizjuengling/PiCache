<!--
  @component
  Adds or edits a reserved address (static lease): MAC address (fixed once
  saved), IPv4 address in the served network, host name, comment, lease
  time (the global one, presets or seconds) and client ID (option 61, hex).
  `preset` fills a new reservation (e.g. from a handed-out address).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type DhcpStaticLease, type DhcpStaticLeaseInput } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime, formatDuration } from '$lib/format'
  import { confirm, Field, Input, KeyValue, Select, toast, type SelectOption } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import NumberInput from '../shared/NumberInput.svelte'

  interface Props {
    open?: boolean
    /** The reservation to edit; omit to add one. */
    lease?: DhcpStaticLease
    /** Initial values of a new reservation. */
    preset?: Partial<DhcpStaticLeaseInput>
    /** The served network, e.g. "192.168.178.0/24". */
    subnet?: string
    /** An example address for the placeholder. */
    example?: string
    /** The global lease time in seconds (dhcp.leaseSeconds). */
    globalLease?: number
    onsaved?: (s: DhcpStaticLease) => void
    ondeleted?: (mac: string) => void
  }

  let { open = $bindable(false), lease, preset, subnet, example, globalLease, onsaved, ondeleted }: Props = $props()

  interface Draft {
    mac: string
    ip: string
    hostname: string
    comment: string
    clientId: string
    /** 0 = the global lease time. */
    leaseSeconds: number
  }

  let draft = $state<Draft>({ mac: '', ip: '', hostname: '', comment: '', clientId: '', leaseSeconds: 0 })
  /** The lease time is typed in seconds instead of chosen from the presets. */
  let custom = $state(false)
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  /** Lease time presets in seconds (as in the set-up). */
  const PRESETS = [3600, 43200, 86400, 604800]

  const leaseMac = $derived(lease?.mac)
  $effect.pre(() => {
    if (!open) return
    void leaseMac
    const p = preset
    untrack(() => {
      const s = lease ?? p
      draft = {
        mac: s?.mac ?? '',
        ip: s?.ip ?? '',
        hostname: s?.hostname ?? '',
        comment: s?.comment ?? '',
        clientId: s?.clientId ?? '',
        leaseSeconds: s?.leaseSeconds ?? 0,
      }
      custom = draft.leaseSeconds > 0 && !PRESETS.includes(draft.leaseSeconds)
      err = undefined
      submitted = false
    })
  })

  // ---- lease time

  const leaseOptions = $derived<SelectOption[]>([
    {
      value: 'global',
      label: globalLease ? t('dns.dhcp.static.leaseGlobal', { duration: formatDuration(globalLease * 1000) }) : t('dns.dhcp.static.leaseGlobalShort'),
    },
    { value: '3600', label: t('dns.dhcp.setup.lease.hour') },
    { value: '43200', label: t('dns.dhcp.setup.lease.halfDay') },
    { value: '86400', label: t('dns.dhcp.setup.lease.day') },
    { value: '604800', label: t('dns.dhcp.setup.lease.week') },
    { value: 'custom', label: t('dns.dhcp.setup.lease.custom') },
  ])

  const leaseChoice = $derived(custom ? 'custom' : draft.leaseSeconds === 0 ? 'global' : String(draft.leaseSeconds))

  function chooseLease(v: string) {
    if (v === 'custom') {
      custom = true
      if (draft.leaseSeconds === 0) draft.leaseSeconds = globalLease ?? 86400
      return
    }
    custom = false
    draft.leaseSeconds = v === 'global' ? 0 : Number(v)
  }

  /** "AA-BB-CC-DD-EE-FF" → "aa:bb:cc:dd:ee:ff" (the server normalises again). */
  function normMac(s: string): string {
    return s.trim().toLowerCase().replace(/-/g, ':')
  }

  const name = $derived(lease?.hostname || lease?.mac || '')
  const macError = $derived(fieldError(err, 'mac') ?? (submitted && !draft.mac.trim() ? t('common.field.required') : undefined))
  const ipError = $derived(fieldError(err, 'ip') ?? (submitted && !draft.ip.trim() ? t('common.field.required') : undefined))
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  async function save() {
    submitted = true
    err = undefined
    if (!draft.mac.trim() || !draft.ip.trim()) return
    // Empty strings (not omitted members): a PUT replaces the reservation, so a cleared name stays cleared.
    const body = {
      ip: draft.ip.trim(),
      hostname: draft.hostname.trim(),
      comment: draft.comment.trim(),
      clientId: draft.clientId.trim(),
      leaseSeconds: draft.leaseSeconds,
    }
    saving = true
    try {
      const saved = lease ? await api.dhcp.static.update(lease.mac, body) : await api.dhcp.static.create({ mac: normMac(draft.mac), ...body })
      toast.success(lease ? t('dns.dhcp.static.saved') : t('dns.dhcp.static.added', { ip: saved.ip }))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function remove() {
    const s = lease
    if (!s) return
    const ok = await confirm({
      title: t('dns.dhcp.static.deleteTitle', { name: s.hostname || s.mac }),
      message: t('dns.dhcp.static.deleteText', { ip: s.ip }),
      confirmLabel: t('dns.dhcp.static.delete'),
      action: () => api.dhcp.static.remove(s.mac),
    })
    if (!ok) return
    toast.success(t('dns.dhcp.static.deleted'))
    open = false
    ondeleted?.(s.mac)
  }
</script>

<FormPanel
  bind:open
  title={lease ? name : t('dns.dhcp.static.addTitle')}
  subtitle={lease ? `${lease.ip} · ${lease.mac}` : undefined}
  submitLabel={lease ? t('common.action.save') : t('dns.dhcp.static.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={lease ? remove : undefined}
  deleteLabel={t('dns.dhcp.static.delete')}
>
  {#snippet header()}
    {#if lease}
      <KeyValue
        items={[
          { label: t('common.label.status'), value: lease.active ? t('dns.dhcp.static.inUse') : t('dns.dhcp.static.unused') },
          { label: t('common.label.created'), value: formatDateTime(lease.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(lease.updatedAt) },
        ]}
      />
    {/if}
  {/snippet}

  <Field
    label={t('dns.dhcp.static.mac')}
    required
    help={lease ? t('dns.dhcp.static.macFixed') : t('dns.dhcp.static.macHelp')}
    error={macError}
  >
    <Input bind:value={draft.mac} mono disabled={!!lease} placeholder="aa:bb:cc:dd:ee:ff" maxlength={32} autocomplete="off" />
  </Field>
  <Field
    label={t('dns.dhcp.static.ip')}
    required
    help={subnet ? t('dns.dhcp.static.ipHelp', { subnet }) : t('dns.dhcp.static.ipHelpNoSubnet')}
    error={ipError}
  >
    <Input bind:value={draft.ip} mono placeholder={example ?? '192.168.1.20'} maxlength={15} autocomplete="off" />
  </Field>
  <Field label={t('dns.dhcp.static.hostname')} optional help={t('dns.dhcp.static.hostnameHelp')} error={fieldError(err, 'hostname')}>
    <Input bind:value={draft.hostname} mono placeholder="printer" maxlength={63} autocomplete="off" />
  </Field>
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={200} />
  </Field>
  <Field
    label={t('dns.dhcp.static.leaseTime')}
    help={custom ? undefined : t('dns.dhcp.static.leaseHelp')}
    error={custom ? undefined : fieldError(err, 'leaseSeconds')}
  >
    <Select bind:value={() => leaseChoice, chooseLease} options={leaseOptions} />
  </Field>
  {#if custom}
    <Field
      label={t('dns.dhcp.setup.leaseSeconds')}
      hideLabel
      help={t('dns.dhcp.setup.leaseCustomHelp', { duration: formatDuration(draft.leaseSeconds * 1000) })}
      error={fieldError(err, 'leaseSeconds')}
    >
      <NumberInput
        bind:value={draft.leaseSeconds}
        min={300}
        max={604800}
        unit={t('dns.shared.unit.seconds')}
        ariaLabel={t('dns.dhcp.setup.leaseSeconds')}
      />
    </Field>
  {/if}
  <Field label={t('dns.dhcp.static.clientId')} optional help={t('dns.dhcp.static.clientIdHelp')} error={fieldError(err, 'clientId')}>
    <Input bind:value={draft.clientId} mono placeholder="01:aa:bb:cc:dd:ee:ff" maxlength={800} autocomplete="off" />
  </Field>
</FormPanel>
