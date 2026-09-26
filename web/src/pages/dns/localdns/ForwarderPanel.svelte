<!--
  @component
  Adds or edits a conditional forwarder: queries for one or more domains
  (each with its subdomains, or only subdomains with "*."), and optionally
  single-label names such as "nas", go to specific DNS servers (the router
  for fritz.box, a company DNS for corp.example) or to the default upstreams
  (public.corp.example while corp.example goes elsewhere).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_TARGET,
    toApiError,
    UNQUALIFIED_DOMAIN,
    type ApiError,
    type Forwarder,
    type ForwarderInput,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, toast } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import FormPanel from '../shared/FormPanel.svelte'
  import { asciiDomain } from '../shared/input'
  import LinesInput from '../shared/LinesInput.svelte'
  import { forwarderDomains, forwarderTitle, usesDefault } from './forwarders'

  interface Props {
    open?: boolean
    forwarder?: Forwarder
    onsaved?: (f: Forwarder) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), forwarder, onsaved, ondeleted }: Props = $props()

  const MAX_TARGETS = 8
  const MAX_DOMAINS = 16

  /** Domains typed one per line (without the single-label choice). */
  let names = $state<string[]>([])
  let unqualified = $state(false)
  let useDefault = $state(false)
  let targets = $state<string[]>([])
  let enabled = $state(true)
  let comment = $state('')
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)
  /** The domains of the last request, to point errors ("domains[2]") at the right line. */
  let sent: string[] = []
  // The single-label choice keeps its place (domains[0] is the forwarder's domain).
  let unqualifiedFirst = false

  const fwdId = $derived(forwarder?.id)
  $effect.pre(() => {
    if (!open) return
    void fwdId
    untrack(() => {
      const f = forwarder
      const domains = f ? forwarderDomains(f) : []
      names = domains.filter((d) => d !== UNQUALIFIED_DOMAIN)
      unqualified = domains.includes(UNQUALIFIED_DOMAIN)
      unqualifiedFirst = domains[0] === UNQUALIFIED_DOMAIN
      useDefault = !!f && usesDefault(f)
      targets = f && !useDefault ? [...f.upstreams] : []
      enabled = f?.enabled ?? true
      comment = f?.comment ?? ''
      err = undefined
      submitted = false
    })
  })

  function domains(): string[] {
    const list = names.map(asciiDomain).filter((d) => d !== UNQUALIFIED_DOMAIN)
    const single = unqualified || names.includes(UNQUALIFIED_DOMAIN)
    if (!single) return list
    return unqualifiedFirst || list.length === 0 ? [UNQUALIFIED_DOMAIN, ...list] : [...list, UNQUALIFIED_DOMAIN]
  }

  // "domains[i]" names an entry of the request: the single-label choice or a line.
  const entryError = $derived.by(() => {
    const m = /^domains\[(\d+)\]$/.exec(err?.field ?? '')
    if (!m || !err) return undefined
    const d = sent[Number(m[1])]
    if (d === UNQUALIFIED_DOMAIN) return { single: true, text: errorText(err) }
    const line = sent.filter((x) => x !== UNQUALIFIED_DOMAIN).indexOf(d ?? '') + 1
    return { single: false, text: line > 0 ? t('dns.shared.lineError', { line, message: errorText(err) }) : errorText(err) }
  })

  const domainError = $derived.by(() => {
    if (entryError) return entryError.single ? undefined : entryError.text
    const own = fieldError(err, 'domains') ?? fieldError(err, 'domain')
    if (own) return own
    const count = names.length + (unqualified ? 1 : 0)
    if (submitted && count === 0) return t('common.field.required')
    if (count > MAX_DOMAINS) return t('dns.forwarders.domainsMax', { max: MAX_DOMAINS })
    return undefined
  })
  const singleError = $derived(entryError?.single ? entryError.text : undefined)

  // With the default upstreams the only target is the keyword: no line to point at.
  const upstreamError = $derived(
    useDefault
      ? fieldError(err, 'upstreams')
      : (lineError(err, 'upstreams') ??
          (submitted && targets.length === 0
            ? t('dns.forwarders.upstreamsRequired')
            : targets.length > MAX_TARGETS
              ? t('dns.forwarders.upstreamsMax', { max: MAX_TARGETS })
              : undefined)),
  )
  // Conflicts (409 "a forwarder for … already exists") and other errors without a field.
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  async function save() {
    submitted = true
    err = undefined
    const list = domains()
    if (list.length === 0 || list.length > MAX_DOMAINS) return
    if (!useDefault && (targets.length === 0 || targets.length > MAX_TARGETS)) return
    saving = true
    sent = list
    try {
      const input: ForwarderInput = {
        domain: list[0],
        domains: list,
        upstreams: useDefault ? [DEFAULT_TARGET] : [...targets],
        enabled,
        comment: comment.trim(),
      }
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
      title: t('dns.forwarders.deleteTitle', { domain: forwarderTitle(f) }),
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
  subtitle={forwarder ? forwarderTitle(forwarder) : undefined}
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

  <Field label={t('dns.forwarders.domains')} required={!unqualified} help={t('dns.forwarders.domainsHelp', { max: MAX_DOMAINS })} error={domainError}>
    <LinesInput bind:value={names} rows={3} placeholder={'fritz.box\n178.168.192.in-addr.arpa'} />
  </Field>
  <div class="stack-sm">
    <Checkbox bind:checked={unqualified} label={t('dns.forwarders.unqualified')} description={t('dns.forwarders.unqualifiedHelp')} />
    {#if singleError}<p class="err">{singleError}</p>{/if}
  </div>

  <div class="stack-sm">
    <Checkbox bind:checked={useDefault} label={t('dns.forwarders.useDefault')} description={t('dns.forwarders.useDefaultHelp')} />
    {#if useDefault && upstreamError}<p class="err">{upstreamError}</p>{/if}
  </div>
  {#if !useDefault}
    <Field label={t('dns.forwarders.upstreams')} required help={t('dns.forwarders.upstreamsHelp', { max: MAX_TARGETS })} error={upstreamError}>
      <LinesInput bind:value={targets} rows={3} placeholder={'192.168.178.1\ntls://dns.example.net'} />
    </Field>
  {/if}
  <Checkbox bind:checked={enabled} label={t('dns.forwarders.enabled')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={comment} maxlength={512} />
  </Field>
</FormPanel>

<style>
  /* Aligned with the checkbox's text. */
  .err {
    padding-left: 24px;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
