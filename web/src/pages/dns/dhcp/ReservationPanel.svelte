<!--
  @component
  Adds or edits a reserved address (static lease): MAC address (fixed once
  saved), IPv4 address in the served network, host name and comment.
  `preset` fills a new reservation (e.g. from a handed-out address).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type DhcpStaticLease, type DhcpStaticLeaseInput } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { confirm, Field, Input, KeyValue, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'

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
    onsaved?: (s: DhcpStaticLease) => void
    ondeleted?: (mac: string) => void
  }

  let { open = $bindable(false), lease, preset, subnet, example, onsaved, ondeleted }: Props = $props()

  interface Draft {
    mac: string
    ip: string
    hostname: string
    comment: string
  }

  let draft = $state<Draft>({ mac: '', ip: '', hostname: '', comment: '' })
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const leaseMac = $derived(lease?.mac)
  $effect.pre(() => {
    if (!open) return
    void leaseMac
    const p = preset
    untrack(() => {
      const s = lease ?? p
      draft = { mac: s?.mac ?? '', ip: s?.ip ?? '', hostname: s?.hostname ?? '', comment: s?.comment ?? '' }
      err = undefined
      submitted = false
    })
  })

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
    const body = { ip: draft.ip.trim(), hostname: draft.hostname.trim(), comment: draft.comment.trim() }
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
</FormPanel>
