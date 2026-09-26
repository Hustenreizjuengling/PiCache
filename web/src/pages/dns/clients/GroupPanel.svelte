<!--
  @component
  Adds or edits a group (name, comment, enabled) and its DNS resolver: the
  default upstreams, a family resolver preset or its own upstreams (with
  their health). The Default group can be renamed and disabled but never
  deleted, and it always uses the upstreams of the DNS settings. A group
  made by "Only for this device" names its device. The resolver of an
  existing group is saved on its own (PUT /groups/{id}/upstreams), so the
  parental controls page and this panel never overwrite each other.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    ApiError,
    DEFAULT_GROUP_ID,
    resource,
    toApiError,
    type Client,
    type ClientGroup,
    type ClientGroupInput,
    type UpstreamPreset,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime, formatNumber } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { Checkbox, confirm, Field, Input, KeyValue, Notice, Select, toast } from '$lib/ui'
  import UpstreamList from '../settings/UpstreamList.svelte'
  import FormPanel from '../shared/FormPanel.svelte'
  import { presetName } from '../shared/resolver'

  interface Props {
    open?: boolean
    group?: ClientGroup
    /** For the name of a device group's client. */
    clients: readonly Client[] | undefined
    presets: readonly UpstreamPreset[] | undefined
    onsaved?: (g: ClientGroup) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), group, clients, presets, onsaved, ondeleted }: Props = $props()

  /** A group's own upstreams (at most 8). */
  const MAX_UPSTREAMS = 8
  const OWN = 'own'

  interface Draft {
    name: string
    comment: string
    enabled: boolean
    /** "" = default upstreams, a preset key, or "own". */
    resolver: string
    upstreams: string[]
  }

  let draft = $state<Draft>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)
  /** The entry of each own upstream last sent (empty entries are left out). */
  let sentIndex = $state.raw<number[]>([])

  function blank(): Draft {
    return { name: '', comment: '', enabled: true, resolver: '', upstreams: [] }
  }

  function resolverOf(g: ClientGroup): string {
    return g.upstreamPreset || (g.upstreams.length > 0 ? OWN : '')
  }

  const groupId = $derived(group?.id)
  $effect.pre(() => {
    if (!open) return
    void groupId
    untrack(() => {
      const g = group
      draft = g ? { name: g.name, comment: g.comment, enabled: g.enabled, resolver: resolverOf(g), upstreams: [...g.upstreams] } : blank()
      err = undefined
      submitted = false
    })
  })

  // The health of the group's own upstreams (only while the panel shows one).
  const upstreamState = resource((signal) => (open && group ? api.upstreams.get({ signal }) : Promise.resolve(undefined)))
  const stats = $derived(upstreamState.data?.groups.find((s) => !!group && s.groupIds.includes(group.id))?.upstreams)

  const isDefault = $derived(group?.id === DEFAULT_GROUP_ID)
  const device = $derived(group?.deviceClientId ? clients?.find((c) => c.id === group.deviceClientId) : undefined)
  const preset = $derived(presets?.find((p) => p.key === draft.resolver))
  const nameError = $derived(fieldError(err, 'name') ?? (submitted && !draft.name.trim() ? t('common.field.required') : undefined))
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  const resolverOptions = $derived([
    { value: '', label: t('dns.resolver.default') },
    ...(presets ?? []).map((p) => ({ value: p.key, label: presetName(p.key, presets) })),
    { value: OWN, label: t('dns.resolver.ownOption') },
  ])

  /** What the resolver fields send: a preset or own upstreams, never both. */
  function resolverInput(d: Draft): { upstreams: string[]; upstreamPreset: string } {
    if (d.resolver === OWN) return { upstreams: d.upstreams.map((u) => u.trim()).filter(Boolean), upstreamPreset: '' }
    return { upstreams: [], upstreamPreset: d.resolver }
  }

  /** The entries of the own upstreams that are sent (resolverInput leaves out empty ones). */
  function sentEntries(d: Draft): number[] {
    return d.resolver === OWN ? d.upstreams.flatMap((u, i) => (u.trim() ? [i] : [])) : []
  }

  /** The save error with "upstreams[i]" of the sent list pointing at the entry it came from. */
  const upstreamsError = $derived.by(() => {
    const m = err?.field ? /^upstreams\[(\d+)\]$/.exec(err.field) : null
    const i = m ? sentIndex[Number(m[1])] : undefined
    return err && i !== undefined ? new ApiError(err.status, err.code, err.message, `upstreams[${i}]`) : err
  })

  function sameResolver(g: ClientGroup, r: { upstreams: string[]; upstreamPreset: string }): boolean {
    return g.upstreamPreset === r.upstreamPreset && g.upstreams.join('\n') === r.upstreams.join('\n')
  }

  async function save() {
    submitted = true
    err = undefined
    if (!draft.name.trim()) return
    saving = true
    try {
      const input: ClientGroupInput = { name: draft.name.trim(), comment: draft.comment.trim(), enabled: draft.enabled }
      const res = isDefault ? undefined : resolverInput(draft)
      sentIndex = sentEntries(draft)
      let saved: ClientGroup
      if (!group) {
        saved = await api.groups.create({ ...input, ...res })
      } else {
        saved = group
        // The resolver first: its checks (bootstrap servers, local names) are the likelier to refuse.
        if (res && !sameResolver(group, res)) saved = await api.groups.setUpstreams(group.id, res)
        if (input.name !== group.name || input.comment !== group.comment || input.enabled !== group.enabled) {
          try {
            saved = await api.groups.update(group.id, input)
          } catch (e) {
            // The resolver is stored already: the table shows it, and the panel says so.
            if (saved !== group) {
              onsaved?.(saved)
              toast.info(t('dns.groups.resolverSavedOnly', { name: group.name }))
            }
            throw e
          }
        }
      }
      toast.success(group ? t('dns.groups.saved', { name: saved.name }) : t('dns.groups.added', { name: saved.name }))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function remove() {
    const g = group
    if (!g || g.id === DEFAULT_GROUP_ID) return
    const ok = await confirm({
      title: t('dns.groups.deleteTitle', { name: g.name }),
      message: t('dns.groups.deleteText'),
      confirmLabel: t('dns.groups.delete'),
      action: () => api.groups.remove(g.id),
    })
    if (!ok) return
    toast.success(t('dns.groups.deleted', { name: g.name }))
    open = false
    ondeleted?.(g.id)
  }
</script>

<FormPanel
  bind:open
  title={group ? group.name : t('dns.groups.addTitle')}
  size={draft.resolver === OWN ? 'lg' : 'md'}
  submitLabel={group ? t('common.action.save') : t('dns.groups.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={group && !isDefault ? remove : undefined}
  deleteLabel={t('dns.groups.delete')}
>
  {#snippet header()}
    {#if group}
      <KeyValue
        items={[
          { label: t('dns.groups.clients'), value: formatNumber(group.clientCount) },
          { label: t('common.label.created'), value: formatDateTime(group.createdAt) },
        ]}
      />
      {#if isDefault}<Notice>{t('dns.groups.defaultNote')}</Notice>{/if}
      {#if group.deviceClientId}
        <Notice tone="info" icon="user" title={t('dns.groups.deviceTitle')}>
          {device ? t('dns.groups.deviceText', { client: device.name }) : t('dns.groups.deviceTextNoName')}
          {#snippet actions()}
            {#if device}
              <a class="small" href={href('/dns/clients', { sel: device.id })}>{t('dns.groups.openDevice')}</a>
            {/if}
          {/snippet}
        </Notice>
      {/if}
    {/if}
  {/snippet}

  <Field label={t('common.label.name')} required error={nameError}>
    <Input bind:value={draft.name} maxlength={64} autocomplete="off" />
  </Field>
  <Checkbox bind:checked={draft.enabled} label={t('dns.groups.enabled')} description={t('dns.groups.enabledHelp')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={512} />
  </Field>

  <section class="stack-sm resolver" aria-labelledby="group-resolver">
    <h3 id="group-resolver">{t('dns.resolver.title')}</h3>
    {#if isDefault}
      <p class="small muted">
        {t('dns.resolver.defaultGroup')}
        <a href={href('/dns/settings', { section: 'upstreams' })}>{t('dns.resolver.openSettings')}</a>
      </p>
    {:else}
      <Field
        label={t('dns.resolver.label')}
        hideLabel
        error={fieldError(err, 'upstreamPreset')}
        help={draft.resolver === OWN ? t('dns.resolver.ownHelp') : preset ? t('dns.resolver.presetHelp') : t('dns.resolver.defaultHelp')}
      >
        <Select bind:value={draft.resolver} options={resolverOptions} />
      </Field>
      {#if preset}
        <ul class="preset mono small">
          {#each preset.upstreams as u (u)}<li>{u}</li>{/each}
        </ul>
      {:else if draft.resolver === OWN}
        <UpstreamList
          bind:value={draft.upstreams}
          error={upstreamsError}
          field="upstreams"
          max={MAX_UPSTREAMS}
          {stats}
          entryLabel={(n) => t('dns.settings.upstreams.entry', { n })}
          addLabel={t('dns.settings.upstreams.add')}
        />
        <p class="small muted">{t('dns.resolver.ownNote')}</p>
      {/if}
    {/if}
  </section>
</FormPanel>

<style>
  .resolver {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .preset {
    margin: 0;
    padding: var(--sp-2) var(--sp-3);
    list-style: none;
    border-radius: var(--r-control);
    background: var(--surface-2);
    overflow-wrap: anywhere;
  }
</style>
