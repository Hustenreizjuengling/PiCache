<!--
  @component
  Adds or edits a group (name, comment, enabled). The Default group can be
  renamed and disabled but never deleted.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, toApiError, type ApiError, type ClientGroup, type ClientGroupInput } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime, formatNumber } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, Notice, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'

  interface Props {
    open?: boolean
    group?: ClientGroup
    onsaved?: (g: ClientGroup) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), group, onsaved, ondeleted }: Props = $props()

  let draft = $state<ClientGroupInput>({ name: '', comment: '', enabled: true })
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const groupId = $derived(group?.id)
  $effect.pre(() => {
    if (!open) return
    void groupId
    untrack(() => {
      const g = group
      draft = g ? { name: g.name, comment: g.comment, enabled: g.enabled } : { name: '', comment: '', enabled: true }
      err = undefined
      submitted = false
    })
  })

  const isDefault = $derived(group?.id === DEFAULT_GROUP_ID)
  const nameError = $derived(fieldError(err, 'name') ?? (submitted && !draft.name.trim() ? t('common.field.required') : undefined))
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  async function save() {
    submitted = true
    err = undefined
    if (!draft.name.trim()) return
    saving = true
    try {
      const input = { ...draft, name: draft.name.trim(), comment: draft.comment.trim() }
      const saved = group ? await api.groups.update(group.id, input) : await api.groups.create(input)
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
    {/if}
  {/snippet}

  <Field label={t('common.label.name')} required error={nameError}>
    <Input bind:value={draft.name} maxlength={64} autocomplete="off" />
  </Field>
  <Checkbox bind:checked={draft.enabled} label={t('dns.groups.enabled')} description={t('dns.groups.enabledHelp')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={512} />
  </Field>
</FormPanel>
