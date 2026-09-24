<!--
  @component
  Adds or edits a local DNS record (A, AAAA, CNAME, TXT). Explains the CNAME
  rules and warns before saving a name that would clash with a CNAME.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type DnsRecord, type DnsRecordInput, type RecordType } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, Notice, Select, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import { asciiDomain } from '../shared/input'
  import NumberInput from '../shared/NumberInput.svelte'

  interface Props {
    open?: boolean
    record?: DnsRecord
    /** All records (for the CNAME clash warning). */
    records: readonly DnsRecord[] | undefined
    onsaved?: (r: DnsRecord) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), record, records, onsaved, ondeleted }: Props = $props()

  const TYPES: RecordType[] = ['A', 'AAAA', 'CNAME', 'TXT']
  const DEFAULT_TTL = 300

  function blank(): DnsRecordInput {
    return { name: '', type: 'A', value: '', ttl: DEFAULT_TTL, enabled: true, comment: '' }
  }

  let draft = $state<DnsRecordInput>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const recordId = $derived(record?.id)
  $effect.pre(() => {
    if (!open) return
    void recordId
    untrack(() => {
      const r = record
      draft = r ? { name: r.name, type: r.type, value: r.value, ttl: r.ttl, enabled: r.enabled, comment: r.comment } : blank()
      err = undefined
      submitted = false
    })
  })

  const valueLabel = $derived(
    {
      A: t('dns.records.value.A'),
      AAAA: t('dns.records.value.AAAA'),
      CNAME: t('dns.records.value.CNAME'),
      TXT: t('dns.records.value.TXT'),
    }[draft.type],
  )
  const valuePlaceholder = $derived({ A: '192.168.1.10', AAAA: 'fd00::10', CNAME: 'nas.lan', TXT: 'v=spf1 -all' }[draft.type])

  /** Another record with the same name that the CNAME rules forbid. */
  const clash = $derived.by(() => {
    const name = asciiDomain(draft.name)
    if (!name || !records) return undefined
    return records.find(
      (r) => r.id !== record?.id && r.name === name && (draft.type === 'CNAME' || r.type === 'CNAME'),
    )
  })

  const nameError = $derived(fieldError(err, 'name') ?? (submitted && !draft.name.trim() ? t('common.field.required') : undefined))
  const valueError = $derived(fieldError(err, 'value') ?? (submitted && !draft.value.trim() ? t('common.field.required') : undefined))
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  async function save() {
    submitted = true
    err = undefined
    if (!draft.name.trim() || !draft.value.trim()) return
    const input: DnsRecordInput = {
      ...draft,
      name: asciiDomain(draft.name),
      value: draft.type === 'CNAME' ? asciiDomain(draft.value) : draft.value.trim(),
      comment: draft.comment.trim(),
    }
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
          { label: t('common.label.created'), value: formatDateTime(record.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(record.updatedAt) },
        ]}
      />
    {/if}
  {/snippet}

  <Field label={t('common.label.name')} required help={t('dns.records.nameHelp')} error={nameError}>
    <Input bind:value={draft.name} mono placeholder="nas.lan" maxlength={253} autocomplete="off" />
  </Field>
  <Field label={t('common.label.type')} error={fieldError(err, 'type')}>
    <Select
      bind:value={() => draft.type, (v) => (draft.type = TYPES.includes(v as RecordType) ? (v as RecordType) : 'A')}
      options={TYPES.map((ty) => ({ value: ty, label: ty }))}
    />
  </Field>
  <Field label={valueLabel} required error={valueError} help={draft.type === 'CNAME' ? t('dns.records.cnameHelp') : undefined}>
    <Input bind:value={draft.value} mono placeholder={valuePlaceholder} maxlength={1024} autocomplete="off" />
  </Field>
  {#if clash}
    <Notice tone="warn">
      {draft.type === 'CNAME'
        ? t('dns.records.clashCname', { name: clash.name, type: clash.type })
        : t('dns.records.clashOther', { name: clash.name })}
    </Notice>
  {/if}
  <Field label={t('dns.records.ttl')} help={t('dns.records.ttlHelp')} error={fieldError(err, 'ttl')}>
    <NumberInput bind:value={draft.ttl} min={0} max={86400} unit={t('dns.shared.unit.seconds')} />
  </Field>
  <Checkbox bind:checked={draft.enabled} label={t('dns.records.enabled')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={512} />
  </Field>
</FormPanel>
