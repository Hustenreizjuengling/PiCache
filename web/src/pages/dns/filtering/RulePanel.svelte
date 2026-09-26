<!--
  @component
  Adds or edits an allow/block rule (exact domain, domain with subdomains or
  regular expression) with its groups, the query types it applies to and,
  for block rules, its answer, exceptions (subtree and regex rules) and
  inversion (regex rules). Used by the rules tab, the query log ("Block
  domain" / "Allow domain"), the tester, and in device mode by the query
  log's "Only for this device": the rule then applies to a group that holds
  only that device, which the server finds or creates.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    ApiError,
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ClientGroup,
    type DeviceRuleResult,
    type FilterRule,
    type FilterRuleInput,
    type RuleReply,
    type RuleType,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { Checkbox, confirm, Field, Input, KeyValue, Notice, Select, toast } from '$lib/ui'
  import AddressInput from '../shared/AddressInput.svelte'
  import { lineError } from '../shared/errors'
  import FormPanel from '../shared/FormPanel.svelte'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import LinesInput from '../shared/LinesInput.svelte'
  import QtypePicker from './QtypePicker.svelte'
  import { blankRule, draftOf, hasDenyallow, hasInvert, hasReply, REPLIES, replyLabel, ruleInput, type RuleDraft } from './rules'

  interface Props {
    open?: boolean
    /** The rule to edit; omit to add a new one. */
    rule?: FilterRule
    /** Initial values for a new rule (e.g. from the query log). */
    preset?: Partial<FilterRuleInput>
    /** Device mode: the rule applies only to this device (POST /filter/rules/device). */
    device?: { ip: string; name?: string }
    groups: readonly ClientGroup[] | undefined
    onsaved?: (rule: FilterRule) => void
    /** Device mode: the client, group and rule the server used or created. */
    ondevice?: (res: DeviceRuleResult) => void
    ondeleted?: (id: number) => void
  }

  let { open = $bindable(false), rule, preset, device, groups, onsaved, ondevice, ondeleted }: Props = $props()

  let draft = $state<RuleDraft>(blankRule())
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
      draft = r ? draftOf(r) : { ...blankRule(), ...stripUndefined(p), groupIds: [...(p?.groupIds ?? [DEFAULT_GROUP_ID])] }
      err = undefined
      submitted = false
    })
  })

  function stripUndefined(p: Partial<FilterRuleInput> | undefined): Partial<RuleDraft> {
    return Object.fromEntries(Object.entries(p ?? {}).filter(([, v]) => v !== undefined)) as Partial<RuleDraft>
  }

  const deviceName = $derived(device ? (device.name ? `${device.name} (${device.ip})` : device.ip) : '')

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
  const replyOptions = $derived(REPLIES.map((r) => ({ value: r, label: replyLabel(r) })))

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
  const replyHelp = $derived(
    draft.reply === ''
      ? t('dns.rules.replyHelp.default')
      : draft.reply === 'custom_ip'
        ? t('dns.rules.replyHelp.custom')
        : t('dns.rules.replyHelp.other'),
  )

  async function save() {
    submitted = true
    err = undefined
    if (!draft.pattern.trim()) return
    saving = true
    try {
      if (device) {
        const res = await api.filter.rules.device({ clientIp: device.ip, rule: ruleInput(draft, false) })
        toast.success(t('dns.rules.device.done', { group: res.group.name }))
        open = false
        ondevice?.(res)
        return
      }
      const input = ruleInput(draft)
      const saved = rule ? await api.filter.rules.update(rule.id, input) : await api.filter.rules.create(input)
      toast.success(rule ? t('dns.rules.saved') : t('dns.rules.added'))
      open = false
      onsaved?.(saved)
    } catch (e) {
      err = deviceError(toApiError(e))
    } finally {
      saving = false
    }
  }

  /** Device mode: "rule.pattern" belongs to the pattern field; a refused address is a general error. */
  function deviceError(e: ApiError): ApiError {
    if (!device || !e.field) return e
    if (e.field.startsWith('rule.')) return new ApiError(e.status, e.code, e.message, e.field.slice(5))
    return new ApiError(e.status, e.code, e.message)
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
  title={device ? t('dns.rules.device.title') : rule ? t('dns.rules.editTitle') : t('dns.rules.addTitle')}
  subtitle={device ? deviceName : rule?.pattern}
  submitLabel={device ? t('dns.rules.device.submit') : rule ? t('common.action.save') : t('dns.rules.add')}
  {saving}
  error={generalError}
  onsubmit={save}
  ondelete={rule && !device ? remove : undefined}
  deleteLabel={t('dns.rules.delete')}
>
  {#snippet header()}
    {#if rule && !device}
      <KeyValue
        items={[
          { label: t('common.label.created'), value: formatDateTime(rule.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(rule.updatedAt) },
        ]}
      />
    {/if}
    {#if device}
      <Notice tone="info" icon="user">{t('dns.rules.device.notice', { client: deviceName })}</Notice>
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

  <QtypePicker bind:value={draft.qtypes} bind:negate={draft.qtypesNegate} error={fieldError(err, 'qtypes') ?? fieldError(err, 'qtypesNegate')} />

  {#if hasReply(draft)}
    <Field label={t('dns.rules.reply')} help={replyHelp} error={fieldError(err, 'reply')}>
      <Select bind:value={() => draft.reply, (v) => (draft.reply = v as RuleReply)} options={replyOptions} />
    </Field>
    {#if draft.reply === 'custom_ip'}
      <div class="two">
        <AddressInput
          bind:value={draft.replyIpv4}
          label={t('dns.rules.replyIpv4')}
          placeholder="192.168.1.2"
          error={fieldError(err, 'replyIpv4')}
          optional
        />
        <AddressInput
          bind:value={draft.replyIpv6}
          label={t('dns.rules.replyIpv6')}
          placeholder="fd00::2"
          error={fieldError(err, 'replyIpv6')}
          optional
        />
      </div>
    {/if}
  {/if}

  {#if hasDenyallow(draft)}
    <Field
      label={t('dns.rules.denyallow')}
      optional
      help={draft.type === 'subtree' ? t('dns.rules.denyallowHelp.subtree') : t('dns.rules.denyallowHelp.regex')}
      error={lineError(err, 'denyallow')}
    >
      <LinesInput bind:value={draft.denyallow} rows={3} placeholder={draft.type === 'subtree' ? 'www.example.com' : 'example.com'} />
    </Field>
  {/if}

  {#if hasInvert(draft)}
    <div class="stack-sm">
      <Checkbox bind:checked={draft.invert} label={t('dns.rules.invert')} description={t('dns.rules.invertHelp')} />
      {#if draft.invert}<Notice tone="warn">{t('dns.rules.invertWarn')}</Notice>{/if}
      {#if fieldError(err, 'invert')}<p class="err">{fieldError(err, 'invert')}</p>{/if}
    </div>
  {/if}

  {#if !device}
    <GroupPicker
      {groups}
      bind:value={draft.groupIds}
      error={fieldError(err, 'groupIds')}
      help={t('dns.rules.groupsHelp')}
    />
  {/if}
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
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  @media (max-width: 480px) {
    .two {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
