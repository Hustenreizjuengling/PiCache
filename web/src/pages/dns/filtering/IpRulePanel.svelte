<!--
  @component
  Adds or edits an IP rule: an address or network that blocks an answer
  containing it (answered like a blocked name, in the global blocking mode)
  or that the lists of answer addresses must not block, with its groups.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, toApiError, type ApiError, type ClientGroup, type IPRule, type IPRuleInput, type RuleAction } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, Select, toast } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import GroupPicker from '../shared/GroupPicker.svelte'

  interface Props {
    open?: boolean
    /** The rule to edit; omit to add a new one. */
    rule?: IPRule
    groups: readonly ClientGroup[] | undefined
    onsaved?: (rule: IPRule) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), rule, groups, onsaved, ondeleted }: Props = $props()

  type Draft = Required<IPRuleInput>

  function blank(): Draft {
    return { action: 'block', pattern: '', enabled: true, groupIds: [DEFAULT_GROUP_ID], comment: '' }
  }

  let draft = $state<Draft>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  const ruleId = $derived(rule?.id)
  $effect.pre(() => {
    if (!open) return
    void ruleId
    untrack(() => {
      const r = rule
      draft = r ? { action: r.action, pattern: r.pattern, enabled: r.enabled, groupIds: [...r.groupIds], comment: r.comment } : blank()
      err = undefined
      submitted = false
    })
  })

  const patternError = $derived(
    fieldError(err, 'pattern') ?? (submitted && !draft.pattern.trim() ? t('common.field.required') : undefined),
  )
  const generalError = $derived(err && !err.field ? errorText(err) : undefined)

  const actionOptions = $derived([
    { value: 'block', label: t('dns.ipRules.action.block') },
    { value: 'allow', label: t('dns.ipRules.action.allow') },
  ])

  async function save() {
    submitted = true
    err = undefined
    if (!draft.pattern.trim()) return
    const input: IPRuleInput = { ...draft, pattern: draft.pattern.trim().toLowerCase(), comment: draft.comment.trim() }
    saving = true
    try {
      const saved = rule ? await api.filter.ipRules.update(rule.id, input) : await api.filter.ipRules.create(input)
      toast.success(rule ? t('dns.ipRules.saved') : t('dns.ipRules.added'))
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
      title: t('dns.ipRules.deleteTitle', { pattern: r.pattern }),
      confirmLabel: t('dns.ipRules.delete'),
      action: () => api.filter.ipRules.remove(r.id),
    })
    if (!ok) return
    toast.success(t('dns.ipRules.deleted'))
    open = false
    ondeleted?.(r.id)
  }
</script>

<FormPanel
  bind:open
  title={rule ? t('dns.ipRules.editTitle') : t('dns.ipRules.addTitle')}
  subtitle={rule?.pattern}
  submitLabel={rule ? t('common.action.save') : t('dns.ipRules.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={rule ? remove : undefined}
  deleteLabel={t('dns.ipRules.delete')}
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

  <Field
    label={t('dns.rules.action')}
    error={fieldError(err, 'action')}
    help={draft.action === 'block' ? t('dns.ipRules.actionHelp.block') : t('dns.ipRules.actionHelp.allow')}
  >
    <Select bind:value={() => draft.action, (v) => (draft.action = v as RuleAction)} options={actionOptions} />
  </Field>
  <Field label={t('dns.ipRules.pattern')} help={t('dns.ipRules.patternHelp')} error={patternError} required>
    <Input bind:value={draft.pattern} mono placeholder="203.0.113.0/24" maxlength={64} autocomplete="off" />
  </Field>
  <GroupPicker {groups} bind:value={draft.groupIds} error={fieldError(err, 'groupIds')} help={t('dns.rules.groupsHelp')} />
  <Checkbox bind:checked={draft.enabled} label={t('dns.rules.enabled')} />
  <Field label={t('common.label.comment')} optional error={fieldError(err, 'comment')}>
    <Input bind:value={draft.comment} maxlength={500} />
  </Field>
</FormPanel>
