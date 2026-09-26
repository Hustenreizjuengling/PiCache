<!--
  @component
  Form fields of a filter list (name, URL, kind, category, plain-domain
  handling, groups, enabled, comment), shared by "Add blocklist" and the
  list panel. The category is fixed to "allow" for allowlists; protection
  categories and allowlists explain how they apply. A new allowlist starts
  without the Default group, so its groups are a deliberate choice.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { CATALOG_CATEGORIES, DEFAULT_GROUP_ID, type ClientGroup, type FilterListInput, type ListCategory } from '$lib/api'
  import { fieldError } from '$lib/errors'
  import { Checkbox, Field, Input, Notice, Select } from '$lib/ui'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import { categoryLabel, isProtection } from './listStatus'

  interface Props {
    draft: FilterListInput
    /** Last save error (field errors are shown next to the fields). */
    err?: unknown
    groups: readonly ClientGroup[] | undefined
    /** Show "required" messages for empty fields. */
    submitted?: boolean
    /** A new list: the category may stay automatic, and an allowlist drops the preselected Default group. */
    fresh?: boolean
  }

  let { draft = $bindable(), err, groups, submitted = false, fresh = false }: Props = $props()

  const kindOptions = $derived([
    { value: 'block', label: t('dns.lists.kind.block') },
    { value: 'allow', label: t('dns.lists.kind.allow') },
  ])
  const plainOptions = $derived([
    { value: 'exact', label: t('dns.lists.plain.exact') },
    { value: 'subtree', label: t('dns.lists.plain.subtree') },
  ])
  const allow = $derived(draft.kind === 'allow')
  const categoryOptions = $derived(
    allow
      ? [{ value: 'allow', label: categoryLabel('allow') }]
      : [
          ...(fresh ? [{ value: '', label: t('dns.lists.categoryAuto') }] : []),
          ...[...CATALOG_CATEGORIES.filter((c) => c !== 'allow'), 'other'].map((c) => ({ value: c, label: categoryLabel(c) })),
        ],
  )

  function setKind(v: string) {
    const kind = v === 'allow' ? 'allow' : 'block'
    if (kind === draft.kind) return
    draft.kind = kind
    // Allowlists always have the category "allow"; a blocklist needs another one.
    if (kind === 'allow') draft.category = 'allow'
    else if (draft.category === 'allow') draft.category = fresh ? '' : 'other'
    if (!fresh) return
    // The Default preselection suits blocklists, not allowlists.
    const onlyDefault = draft.groupIds.length === 1 && draft.groupIds[0] === DEFAULT_GROUP_ID
    if (kind === 'allow' && onlyDefault) draft.groupIds = []
    else if (kind === 'block' && draft.groupIds.length === 0) draft.groupIds = [DEFAULT_GROUP_ID]
  }
</script>

<Field label={t('common.label.name')} error={fieldError(err, 'name')} optional help={t('dns.lists.nameHelp')}>
  <Input bind:value={draft.name} maxlength={100} />
</Field>
<Field
  label={t('dns.lists.url')}
  required
  help={t('dns.lists.urlHelp')}
  error={fieldError(err, 'url') ?? (submitted && !draft.url.trim() ? t('common.field.required') : undefined)}
>
  <Input bind:value={draft.url} mono type="url" maxlength={2048} placeholder="https://" inputmode="url" />
</Field>
<Field label={t('dns.lists.kind')} error={fieldError(err, 'kind')} help={t('dns.lists.kindHelp')}>
  <Select bind:value={() => draft.kind, setKind} options={kindOptions} />
</Field>
{#if allow}
  <Notice tone="info">{t('dns.lists.allowNotice')}</Notice>
{/if}
<Field
  label={t('dns.lists.categoryLabel')}
  error={fieldError(err, 'category')}
  help={isProtection(draft.category) ? t('dns.lists.protectionNotice') : allow ? undefined : t('dns.lists.categoryHelp')}
>
  <Select
    bind:value={() => (allow ? 'allow' : (draft.category ?? '')), (v) => (draft.category = v as ListCategory | '')}
    options={categoryOptions}
    disabled={allow}
  />
</Field>
<Field label={t('dns.lists.plain')} error={fieldError(err, 'plainDomains')} help={t('dns.lists.plainHelp')}>
  <Select
    bind:value={() => draft.plainDomains, (v) => (draft.plainDomains = v === 'subtree' ? 'subtree' : 'exact')}
    options={plainOptions}
  />
</Field>
<GroupPicker
  {groups}
  bind:value={draft.groupIds}
  error={fieldError(err, 'groupIds')}
  help={t('dns.lists.groupsHelp')}
  emptyWarning={allow && fresh ? t('dns.lists.allowGroupsRequired') : undefined}
/>
<Checkbox bind:checked={draft.enabled} label={t('dns.lists.enabled')} />
<Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
  <Input bind:value={draft.comment} maxlength={500} />
</Field>
