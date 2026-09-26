<!--
  @component
  Details of a filter list (category, download state, entries, skipped
  lines, last error) with its settings, "Update now" and Delete. A
  protection list says that it is enforced like parental controls.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type ClientGroup, type FilterList, type FilterListInput } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, confirm, KeyValue, Notice, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import ListFields from './ListFields.svelte'
  import { categoryLabel, isProtection, listInput, listStatus } from './listStatus'

  interface Props {
    open?: boolean
    list: FilterList | undefined
    groups: readonly ClientGroup[] | undefined
    /** A list was changed (saved or updated): the new state. */
    onchanged?: (list: FilterList) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), list, groups, onchanged, ondeleted }: Props = $props()

  let draft = $state<FilterListInput>({
    name: '',
    url: '',
    kind: 'block',
    plainDomains: 'exact',
    enabled: true,
    groupIds: [],
    comment: '',
    category: '',
  })
  let saving = $state(false)
  let refreshing = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  // Fresh values when the panel opens or shows another list (not when the
  // list is merely polled again while the panel is open).
  const listId = $derived(list?.id)
  $effect.pre(() => {
    if (!open) return
    void listId
    untrack(() => {
      if (list) draft = listInput(list)
      err = undefined
      submitted = false
    })
  })

  const status = $derived(list ? listStatus(list.status) : undefined)
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  async function save() {
    const l = list
    if (!l) return
    submitted = true
    err = undefined
    if (!draft.url.trim()) return
    saving = true
    try {
      const saved = await api.filter.lists.update(l.id, { ...draft, name: draft.name.trim(), url: draft.url.trim() })
      toast.success(t('dns.lists.saved'))
      open = false
      onchanged?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function refresh() {
    const l = list
    if (!l) return
    refreshing = true
    try {
      const updated = await api.filter.lists.refresh(l.id)
      onchanged?.(updated)
      if (updated.status === 'failed-cached' || updated.status === 'failed-empty') {
        toast.error(updated.lastError || listStatus(updated.status).label)
      } else {
        toast.success(tn('dns.lists.updated', updated.entries, { count: formatNumber(updated.entries) }))
      }
    } catch (e) {
      toast.error(e)
    } finally {
      refreshing = false
    }
  }

  async function remove() {
    const l = list
    if (!l) return
    const ok = await confirm({
      title: t('dns.lists.deleteTitle', { name: l.name }),
      message: t('dns.lists.deleteText'),
      confirmLabel: t('dns.lists.delete'),
      action: () => api.filter.lists.remove(l.id),
    })
    if (!ok) return
    toast.success(t('dns.lists.deleted'))
    open = false
    ondeleted?.(l.id)
  }
</script>

<FormPanel
  bind:open
  title={list?.name ?? ''}
  subtitle={list?.url}
  size="lg"
  submitLabel={t('common.action.save')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={remove}
  deleteLabel={t('dns.lists.delete')}
>
  {#snippet header()}
    {#if list && status}
      <div class="row">
        <Chip tone={status.tone} label={status.label} />
        {#if !list.enabled}<Chip label={t('common.state.disabled')} />{/if}
      </div>
      {#if list.lastError}
        <Notice tone={list.status === 'failed-empty' ? 'fail' : 'warn'} title={t('dns.lists.lastError')}>
          <span class="mono small">{list.lastError}</span>
        </Notice>
      {/if}
      {#if list.kind === 'block' && isProtection(list.category)}
        <Notice tone="info" icon="shield">{t('dns.lists.protectionNotice')}</Notice>
      {/if}
      {#if list.tldBlocksIgnored > 0}
        <Notice tone="warn">{tn('dns.lists.tldIgnored', list.tldBlocksIgnored)}</Notice>
      {/if}
      <KeyValue
        items={[
          { label: t('dns.lists.categoryLabel'), value: categoryLabel(list.category || 'other') },
          { label: t('dns.lists.entries'), value: formatNumber(list.entries) },
          { label: t('dns.lists.invalid'), value: formatNumber(list.invalid) },
          { label: t('dns.lists.unsupported'), value: formatNumber(list.unsupported) },
          { label: t('dns.lists.size'), value: list.sizeBytes ? formatBytes(list.sizeBytes) : undefined },
          { label: t('dns.lists.lastUpdated'), value: list.lastUpdated ? formatDateTime(list.lastUpdated) : t('common.state.never') },
          { label: t('dns.lists.lastChecked'), value: list.lastChecked ? formatRelative(list.lastChecked) : t('common.state.never') },
          { label: t('dns.lists.lastSuccess'), value: list.lastSuccess ? formatRelative(list.lastSuccess) : t('common.state.never') },
          { label: t('common.label.created'), value: formatDateTime(list.createdAt) },
        ]}
      />
      <h3 class="section">{t('dns.lists.settings')}</h3>
    {/if}
  {/snippet}

  {#snippet extra()}
    <Button icon="refresh" loading={refreshing} disabled={!session.canOperate || saving || !list?.enabled} onclick={refresh}>
      {t('dns.lists.updateNow')}
    </Button>
  {/snippet}

  <ListFields bind:draft {err} {groups} {submitted} />
</FormPanel>

<style>
  .section {
    font-size: var(--fs-md);
    padding-top: var(--sp-2);
    border-top: 1px solid var(--line);
  }
</style>
