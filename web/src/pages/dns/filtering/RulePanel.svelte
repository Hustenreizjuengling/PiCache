<!--
  @component
  Adds or edits an allow/block rule (exact domain, domain with subdomains or
  regular expression) with its groups. Used by the rules tab, the query log
  ("Block domain" / "Allow domain") and the tester.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type ClientGroup,
    type FilterRule,
    type FilterRuleInput,
    type RuleType,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, Select, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import { asciiDomain } from '../shared/input'

  interface Props {
    open?: boolean
    /** The rule to edit; omit to add a new one. */
    rule?: FilterRule
    /** Initial values for a new rule (e.g. from the query log). */
    preset?: Partial<FilterRuleInput>
    groups: readonly ClientGroup[] | undefined
    onsaved?: (rule: FilterRule) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), rule, preset, groups, onsaved, ondeleted }: Props = $props()

  function blank(): FilterRuleInput {
    return { action: 'block', type: 'exact', pattern: '', enabled: true, groupIds: [DEFAULT_GROUP_ID], comment: '' }
  }

  let draft = $state<FilterRuleInput>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  // A fresh form every time the panel opens (or shows another rule).
  const ruleId = $derived(rule?.id)
  $effect.pre(() => {
    if (!open) return
    void ruleId
    const p = preset
    untrack(() => {
      const r = rule
      draft = r
        ? { action: r.action, type: r.type, pattern: r.pattern, enabled: r.enabled, groupIds: [...r.groupIds], comment: r.comment }
        : { ...blank(), ...p, groupIds: [...(p?.groupIds ?? [DEFAULT_GROUP_ID])] }
      err = undefined
      submitted = false
    })
  })

  const patternError = $derived(
    fieldError(err, 'pattern') ?? (submitted && !draft.pattern.trim() ? t('common.field.required') : undefined),
  )
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  const typeOptions = $derived([
    { value: 'exact', label: t('dns.rules.type.exact') },
    { value: 'subtree', label: t('dns.rules.type.subtree') },
    { value: 'regex', label: t('dns.rules.type.regex') },
  ])
  const actionOptions = $derived([
    { value: 'block', label: t('dns.rules.action.block') },
    { value: 'allow', label: t('dns.rules.action.allow') },
  ])

  const typeHelp = $derived(
    draft.type === 'exact'
      ? t('dns.rules.typeHelp.exact')
      : draft.type === 'subtree'
        ? t('dns.rules.typeHelp.subtree')
        : t('dns.rules.typeHelp.regex'),
  )
  const placeholder = $derived(
    draft.type === 'regex' ? '(^|\\.)ads?[0-9]*\\.' : draft.type === 'subtree' ? 'example.com' : 'ads.example.com',
  )

  async function save() {
    submitted = true
    err = undefined
    if (!draft.pattern.trim()) return
    const input: FilterRuleInput = {
      ...draft,
      pattern: draft.type === 'regex' ? draft.pattern.trim() : asciiDomain(draft.pattern),
      comment: draft.comment.trim(),
    }
    saving = true
    try {
      const saved = rule ? await api.filter.rules.update(rule.id, input) : await api.filter.rules.create(input)
      toast.success(rule ? t('dns.rules.saved') : t('dns.rules.added'))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  async function remove() {
    const r = rule
    if (!r) return
    const ok = await confirm({
      title: t('dns.rules.deleteTitle', { pattern: r.pattern }),
      message: t('dns.rules.deleteText'),
      confirmLabel: t('dns.rules.delete'),
      action: () => api.filter.rules.remove(r.id),
    })
    if (!ok) return
    toast.success(t('dns.rules.deleted'))
    open = false
    ondeleted?.(r.id)
  }
</script>

<FormPanel
  bind:open
  title={rule ? t('dns.rules.editTitle') : t('dns.rules.addTitle')}
  subtitle={rule?.pattern}
  submitLabel={rule ? t('common.action.save') : t('dns.rules.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={rule ? remove : undefined}
  deleteLabel={t('dns.rules.delete')}
>
  {#snippet header()}
    {#if rule}
      <KeyValue
        items={[
          { label: t('common.label.created'), value: formatDateTime(rule.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(rule.updatedAt) },
        ]}
      />
    {/if}
  {/snippet}

  <div class="two">
    <Field label={t('dns.rules.action')} error={fieldError(err, 'action')}>
      <Select bind:value={draft.action} options={actionOptions} />
    </Field>
    <Field label={t('common.label.type')} error={fieldError(err, 'type')}>
      <Select bind:value={() => draft.type, (v) => (draft.type = v as RuleType)} options={typeOptions} />
    </Field>
  </div>
  <Field label={t('dns.rules.pattern')} help={typeHelp} error={patternError} required>
    <Input bind:value={draft.pattern} mono {placeholder} maxlength={1100} autocomplete="off" />
  </Field>
  <GroupPicker
    {groups}
    bind:value={draft.groupIds}
    error={fieldError(err, 'groupIds')}
    help={t('dns.rules.groupsHelp')}
  />
  <Checkbox bind:checked={draft.enabled} label={t('dns.rules.enabled')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={500} />
  </Field>
</FormPanel>

<style>
  .two {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-3);
  }
  @media (max-width: 480px) {
    .two {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
