<!--
  @component
  Adds or edits a conditional forwarder: queries for a domain (and its
  subdomains, or only subdomains with "*.") go to specific DNS servers,
  e.g. the router for fritz.box or a company DNS for corp.example.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type Forwarder, type ForwarderInput } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, toast } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import FormPanel from '../shared/FormPanel.svelte'
  import { asciiDomain } from '../shared/input'
  import LinesInput from '../shared/LinesInput.svelte'

  interface Props {
    open?: boolean
    forwarder?: Forwarder
    onsaved?: (f: Forwarder) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), forwarder, onsaved, ondeleted }: Props = $props()

  const MAX_TARGETS = 8

  let draft = $state<ForwarderInput>({ domain: '', upstreams: [], enabled: true, comment: '' })
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const fwdId = $derived(forwarder?.id)
  $effect.pre(() => {
    if (!open) return
    void fwdId
    untrack(() => {
      const f = forwarder
      draft = f
        ? { domain: f.domain, upstreams: [...f.upstreams], enabled: f.enabled, comment: f.comment }
        : { domain: '', upstreams: [], enabled: true, comment: '' }
      err = undefined
      submitted = false
    })
  })

  const domainError = $derived(
    fieldError(err, 'domain') ?? (submitted && !draft.domain.trim() ? t('common.field.required') : undefined),
  )
  const upstreamError = $derived(
    lineError(err, 'upstreams') ??
      (submitted && draft.upstreams.length === 0
        ? t('dns.forwarders.upstreamsRequired')
        : draft.upstreams.length > MAX_TARGETS
          ? t('dns.forwarders.upstreamsMax', { max: MAX_TARGETS })
          : undefined),
  )
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  async function save() {
    submitted = true
    err = undefined
    if (!draft.domain.trim() || draft.upstreams.length === 0 || draft.upstreams.length > MAX_TARGETS) return
    saving = true
    try {
      const input = { ...draft, domain: asciiDomain(draft.domain), comment: draft.comment.trim() }
      const saved = forwarder ? await api.dns.forwarders.update(forwarder.id, input) : await api.dns.forwarders.create(input)
      toast.success(forwarder ? t('dns.forwarders.saved') : t('dns.forwarders.added'))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function remove() {
    const f = forwarder
    if (!f) return
    const ok = await confirm({
      title: t('dns.forwarders.deleteTitle', { domain: f.domain }),
      confirmLabel: t('dns.forwarders.delete'),
      action: () => api.dns.forwarders.remove(f.id),
    })
    if (!ok) return
    toast.success(t('dns.forwarders.deleted'))
    open = false
    ondeleted?.(f.id)
  }
</script>

<FormPanel
  bind:open
  title={forwarder ? t('dns.forwarders.editTitle') : t('dns.forwarders.addTitle')}
  subtitle={forwarder?.domain}
  submitLabel={forwarder ? t('common.action.save') : t('dns.forwarders.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={forwarder ? remove : undefined}
  deleteLabel={t('dns.forwarders.delete')}
>
  {#snippet header()}
    {#if forwarder}
      <KeyValue
        items={[
          { label: t('common.label.created'), value: formatDateTime(forwarder.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(forwarder.updatedAt) },
        ]}
      />
    {/if}
  {/snippet}

  <Field label={t('common.label.domain')} required help={t('dns.forwarders.domainHelp')} error={domainError}>
    <Input bind:value={draft.domain} mono placeholder="fritz.box" maxlength={253} autocomplete="off" />
  </Field>
  <Field label={t('dns.forwarders.upstreams')} required help={t('dns.forwarders.upstreamsHelp', { max: MAX_TARGETS })} error={upstreamError}>
    <LinesInput bind:value={draft.upstreams} rows={3} placeholder={'192.168.178.1\ntls://dns.example.net'} />
  </Field>
  <Checkbox bind:checked={draft.enabled} label={t('dns.forwarders.enabled')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={512} />
  </Field>
</FormPanel>
