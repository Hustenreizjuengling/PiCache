<!--
  @component
  Adds or edits a local DNS record: A, AAAA, CNAME and TXT with a value;
  SRV, MX, PTR, HTTPS and SVCB with one field per part. A record answers
  everyone or the clients of some groups (split horizon; without a group it
  answers nobody); A and AAAA records choose what a query for the other
  address family gets. Explains the CNAME rules and warns before saving a
  name that would clash with a CNAME of the same scope.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type ClientGroup, type DnsRecord, type OtherFamily, type RecordScope, type RecordType } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, Notice, Select, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import { asciiDomain } from '../shared/input'
  import NumberInput from '../shared/NumberInput.svelte'
  import {
    blankRecord,
    draftOfRecord,
    isAddress,
    isStructured,
    RECORD_TYPES,
    recordInput,
    sameScope,
    validSvcPort,
    type RecordDraft,
  } from './records'

  interface Props {
    open?: boolean
    record?: DnsRecord
    /** All records (for the CNAME clash warning). */
    records: readonly DnsRecord[] | undefined
    groups: readonly ClientGroup[] | undefined
    onsaved?: (r: DnsRecord) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), record, records, groups, onsaved, ondeleted }: Props = $props()

  let draft = $state<RecordDraft>(blankRecord())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const recordId = $derived(record?.id)
  $effect.pre(() => {
    if (!open) return
    void recordId
    untrack(() => {
      const r = record
      draft = r ? draftOfRecord(r) : blankRecord()
      err = undefined
      submitted = false
    })
  })

  const structured = $derived(isStructured(draft.type))
  const svcb = $derived(draft.type === 'HTTPS' || draft.type === 'SVCB')

  const valueLabel = $derived(
    {
      A: t('dns.records.value.A'),
      AAAA: t('dns.records.value.AAAA'),
      CNAME: t('dns.records.value.CNAME'),
      TXT: t('dns.records.value.TXT'),
    }[draft.type as 'A' | 'AAAA' | 'CNAME' | 'TXT'] ?? t('dns.records.value'),
  )
  const valuePlaceholder = $derived(
    ({ A: '192.168.1.10', AAAA: 'fd00::10', CNAME: 'nas.lan', TXT: 'v=spf1 -all' } as Record<string, string>)[draft.type] ?? '',
  )
  const namePlaceholder = $derived(
    draft.type === 'SRV' ? '_sip._tcp.example.lan' : draft.type === 'PTR' ? '10.1.168.192.in-addr.arpa' : 'nas.lan',
  )
  const typeHelp = $derived(
    ({
      SRV: t('dns.records.typeHelp.SRV'),
      MX: t('dns.records.typeHelp.MX'),
      PTR: t('dns.records.typeHelp.PTR'),
      HTTPS: t('dns.records.typeHelp.HTTPS'),
      SVCB: t('dns.records.typeHelp.SVCB'),
    } as Record<string, string>)[draft.type],
  )

  /** Another record with the same name, in the same scope, that the CNAME rules forbid. */
  const clash = $derived.by(() => {
    const name = asciiDomain(draft.name)
    if (!name || !records) return undefined
    const scope = { scope: draft.scope, groupIds: draft.groupIds }
    return records.find(
      (r) => r.id !== record?.id && r.name === name && (draft.type === 'CNAME' || r.type === 'CNAME') && sameScope(r, scope),
    )
  })

  const nameError = $derived(fieldError(err, 'name') ?? (submitted && !draft.name.trim() ? t('common.field.required') : undefined))
  const valueError = $derived(
    fieldError(err, 'value') ?? (submitted && !structured && !draft.value.trim() ? t('common.field.required') : undefined),
  )
  const dataError = (member: string) => fieldError(err, `data.${member}`)
  /** A typed HTTPS/SVCB port that is not a number is never sent (it would be dropped as null). */
  const portBad = $derived(svcb && draft.priority > 0 && !validSvcPort(draft.svcPort))
  const portError = $derived(dataError('port') ?? (portBad ? t('dns.records.data.portInvalid') : undefined))
  /** A structured value refused as a whole: its presentation form ("value") or undecodable data ("data"). */
  const structuredError = $derived(fieldError(err, 'value') ?? (err?.field === 'data' ? errorText(err) : undefined))
  const targetError = $derived(dataError('target') ?? (submitted && structured && !draft.target.trim() && draft.type !== 'MX' ? t('common.field.required') : undefined))
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  const scopeOptions = $derived([
    { value: 'all', label: t('dns.records.scope.all') },
    { value: 'groups', label: t('dns.records.scope.groups') },
  ])
  const familyOptions = $derived([
    { value: 'nodata', label: t('dns.records.otherFamily.nodata') },
    { value: 'forward', label: t('dns.records.otherFamily.forward') },
  ])

  function incomplete(): boolean {
    if (!draft.name.trim()) return true
    if (!structured) return !draft.value.trim()
    if (draft.type === 'MX') return !draft.host.trim()
    return !draft.target.trim() || portBad
  }

  async function save() {
    submitted = true
    err = undefined
    if (incomplete()) return
    const input = recordInput(draft)
    saving = true
    try {
      const saved = record ? await api.dns.records.update(record.id, input) : await api.dns.records.create(input)
      toast.success(record ? t('dns.records.saved') : t('dns.records.added'))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function remove() {
    const r = record
    if (!r) return
    const ok = await confirm({
      title: t('dns.records.deleteTitle', { name: r.name, type: r.type }),
      confirmLabel: t('dns.records.delete'),
      action: () => api.dns.records.remove(r.id),
    })
    if (!ok) return
    toast.success(t('dns.records.deleted'))
    open = false
    ondeleted?.(r.id)
  }
</script>

<FormPanel
  bind:open
  title={record ? t('dns.records.editTitle') : t('dns.records.addTitle')}
  subtitle={record ? `${record.name} ${record.type}` : undefined}
  submitLabel={record ? t('common.action.save') : t('dns.records.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={record ? remove : undefined}
  deleteLabel={t('dns.records.delete')}
>
  {#snippet header()}
    {#if record}
      <KeyValue
        items={[
          ...(structured ? [{ label: t('dns.records.value'), value: record.value, mono: true }] : []),
          { label: t('common.label.created'), value: formatDateTime(record.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(record.updatedAt) },
        ]}
      />
    {/if}
  {/snippet}

  <Field label={t('common.label.type')} error={fieldError(err, 'type')} help={typeHelp}>
    <Select
      bind:value={() => draft.type, (v) => (draft.type = RECORD_TYPES.includes(v as RecordType) ? (v as RecordType) : 'A')}
      options={RECORD_TYPES.map((ty) => ({ value: ty, label: ty }))}
    />
  </Field>
  <Field
    label={t('common.label.name')}
    required
    help={draft.type === 'PTR' ? t('dns.records.nameHelpPtr') : t('dns.records.nameHelp')}
    error={nameError}
  >
    <Input bind:value={draft.name} mono placeholder={namePlaceholder} maxlength={253} autocomplete="off" />
  </Field>

  {#if !structured}
    <Field label={valueLabel} required error={valueError} help={draft.type === 'CNAME' ? t('dns.records.cnameHelp') : undefined}>
      <Input bind:value={draft.value} mono placeholder={valuePlaceholder} maxlength={1024} autocomplete="off" />
    </Field>
  {:else}
    {#if structuredError}<Notice tone="fail">{structuredError}</Notice>{/if}
    {#if draft.type === 'SRV'}
      <div class="three">
        <Field label={t('dns.records.data.priority')} error={dataError('priority')}>
          <NumberInput bind:value={draft.priority} min={0} max={65535} />
        </Field>
        <Field label={t('dns.records.data.weight')} error={dataError('weight')}>
          <NumberInput bind:value={draft.weight} min={0} max={65535} />
        </Field>
        <Field label={t('dns.records.data.port')} error={dataError('port')}>
          <NumberInput bind:value={draft.port} min={0} max={65535} />
        </Field>
      </div>
      <Field label={t('dns.records.data.target')} required help={t('dns.records.data.targetHelpSrv')} error={targetError}>
        <Input bind:value={draft.target} mono placeholder="sip.example.lan" maxlength={253} autocomplete="off" />
      </Field>
    {:else if draft.type === 'MX'}
      <div class="two">
        <Field label={t('dns.records.data.preference')} error={dataError('preference')}>
          <NumberInput bind:value={draft.preference} min={0} max={65535} />
        </Field>
      </div>
      <Field
        label={t('dns.records.data.host')}
        required
        help={t('dns.records.data.hostHelp')}
        error={dataError('host') ?? (submitted && !draft.host.trim() ? t('common.field.required') : undefined)}
      >
        <Input bind:value={draft.host} mono placeholder="mail.example.lan" maxlength={253} autocomplete="off" />
      </Field>
    {:else if draft.type === 'PTR'}
      <Field label={t('dns.records.data.target')} required help={t('dns.records.data.targetHelpPtr')} error={targetError}>
        <Input bind:value={draft.target} mono placeholder="printer.lan" maxlength={253} autocomplete="off" />
      </Field>
    {:else if svcb}
      <div class="two">
        <Field label={t('dns.records.data.priority')} help={t('dns.records.data.priorityHelpSvcb')} error={dataError('priority')}>
          <NumberInput bind:value={draft.priority} min={0} max={65535} />
        </Field>
      </div>
      <Field label={t('dns.records.data.target')} required help={t('dns.records.data.targetHelpSvcb')} error={targetError}>
        <Input bind:value={draft.target} mono placeholder="." maxlength={253} autocomplete="off" />
      </Field>
      {#if draft.priority > 0}
        <div class="two">
          <Field label={t('dns.records.data.alpn')} optional help={t('dns.records.data.alpnHelp')} error={dataError('alpn')}>
            <Input bind:value={draft.alpn} mono placeholder="h2, h3" maxlength={200} autocomplete="off" />
          </Field>
          <Field label={t('dns.records.data.port')} optional error={portError}>
            <Input bind:value={draft.svcPort} mono inputmode="numeric" placeholder="443" maxlength={5} autocomplete="off" />
          </Field>
        </div>
        <Field label={t('dns.records.data.ipv4hint')} optional help={t('dns.records.data.hintHelp')} error={dataError('ipv4hint')}>
          <Input bind:value={draft.ipv4hint} mono placeholder="192.168.1.10" maxlength={200} autocomplete="off" />
        </Field>
        <Field label={t('dns.records.data.ipv6hint')} optional help={t('dns.records.data.hintHelp')} error={dataError('ipv6hint')}>
          <Input bind:value={draft.ipv6hint} mono placeholder="fd00::10" maxlength={400} autocomplete="off" />
        </Field>
      {/if}
    {/if}
  {/if}

  {#if clash}
    <Notice tone="warn">
      {draft.type === 'CNAME'
        ? t('dns.records.clashCname', { name: clash.name, type: clash.type })
        : t('dns.records.clashOther', { name: clash.name })}
    </Notice>
  {/if}

  {#if isAddress(draft.type)}
    <Field
      label={t('dns.records.otherFamily')}
      error={fieldError(err, 'otherFamily')}
      help={draft.otherFamily === 'forward' ? t('dns.records.otherFamilyHelp.forward') : t('dns.records.otherFamilyHelp.nodata')}
    >
      <Select
        bind:value={() => draft.otherFamily, (v) => (draft.otherFamily = v === 'forward' ? 'forward' : ('nodata' as OtherFamily))}
        options={familyOptions}
      />
    </Field>
  {/if}

  <Field label={t('dns.records.scope')} error={fieldError(err, 'scope')} help={draft.scope === 'groups' ? t('dns.records.scopeHelp.groups') : t('dns.records.scopeHelp.all')}>
    <Select
      bind:value={() => draft.scope, (v) => (draft.scope = v === 'groups' ? 'groups' : ('all' as RecordScope))}
      options={scopeOptions}
    />
  </Field>
  {#if draft.scope === 'groups'}
    <GroupPicker {groups} bind:value={draft.groupIds} error={fieldError(err, 'groupIds')} emptyWarning={t('dns.records.nobodyWarning')} />
  {/if}

  <Field label={t('dns.records.ttl')} help={t('dns.records.ttlHelp')} error={fieldError(err, 'ttl')}>
    <NumberInput bind:value={draft.ttl} min={0} max={86400} unit={t('dns.shared.unit.seconds')} />
  </Field>
  <Checkbox bind:checked={draft.enabled} label={t('dns.records.enabled')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={512} />
  </Field>
</FormPanel>

<style>
  .two,
  .three {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-3);
  }
  .three {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
  @media (max-width: 480px) {
    .two {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
