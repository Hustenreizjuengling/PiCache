<!--
  @component
  Form fields of a filter list (name, URL, kind, plain-domain handling,
  groups, enabled, comment), shared by "Add blocklist" and the list panel.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ClientGroup, FilterListInput } from '$lib/api'
  import { fieldError } from '$lib/errors'
  import { Checkbox, Field, Input, Select } from '$lib/ui'
  import GroupPicker from '../shared/GroupPicker.svelte'

  interface Props {
    draft: FilterListInput
    /** Last save error (field errors are shown next to the fields). */
    err?: unknown
    groups: readonly ClientGroup[] | undefined
    /** Show "required" messages for empty fields. */
    submitted?: boolean
  }

  let { draft = $bindable(), err, groups, submitted = false }: Props = $props()

  const kindOptions = $derived([
    { value: 'block', label: t('dns.lists.kind.block') },
    { value: 'allow', label: t('dns.lists.kind.allow') },
  ])
  const plainOptions = $derived([
    { value: 'exact', label: t('dns.lists.plain.exact') },
    { value: 'subtree', label: t('dns.lists.plain.subtree') },
  ])
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
  <Select bind:value={() => draft.kind, (v) => (draft.kind = v === 'allow' ? 'allow' : 'block')} options={kindOptions} />
</Field>
<Field label={t('dns.lists.plain')} error={fieldError(err, 'plainDomains')} help={t('dns.lists.plainHelp')}>
  <Select
    bind:value={() => draft.plainDomains, (v) => (draft.plainDomains = v === 'subtree' ? 'subtree' : 'exact')}
    options={plainOptions}
  />
</Field>
<GroupPicker {groups} bind:value={draft.groupIds} error={fieldError(err, 'groupIds')} help={t('dns.lists.groupsHelp')} />
<Checkbox bind:checked={draft.enabled} label={t('dns.lists.enabled')} />
<Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
  <Input bind:value={draft.comment} maxlength={500} />
</Field>
