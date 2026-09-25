<!--
  @component
  Edits the restrictions of one group in a side panel: the services that are
  always blocked and up to 10 schedules (add, edit inline, turn on/off,
  delete). Everything is sent with "Save changes" (PUT /parental/groups/{id});
  validation errors of the server open the schedule they name.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type GroupControls,
    type GroupControlsInput,
    type ParentalSchedule,
    type ParentalService,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, IconButton, Notice, toast, Toggle } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import { blankSchedule, blockText, daysText, MAX_SCHEDULES, normalizeDays, scheduleProblems, windowText } from './plan'
  import ScheduleEditor from './ScheduleEditor.svelte'
  import ServicePicker from './ServicePicker.svelte'

  type FieldName = 'name' | 'days' | 'start' | 'end' | 'block' | 'services'

  interface Editing {
    /** Index in draft.schedules; = length for a new schedule. */
    index: number
    draft: ParentalSchedule
    isNew: boolean
    touched: boolean
    serverError?: { field: FieldName; message: string }
  }

  interface Props {
    open?: boolean
    group: GroupControls | undefined
    catalog: readonly ParentalService[] | undefined
    catalogError?: ApiError
    onretrycatalog?: () => void
    onsaved?: (g: GroupControls) => void
  }

  let { open = $bindable(false), group, catalog, catalogError, onretrycatalog, onsaved }: Props = $props()

  let draft = $state<GroupControlsInput>({ blockedServices: [], schedules: [] })
  let editing = $state<Editing | null>(null)
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)

  function copy(s: ParentalSchedule): ParentalSchedule {
    return { ...s, days: [...(s.days ?? [])], services: [...(s.services ?? [])] }
  }

  const groupId = $derived(group?.groupId)
  $effect.pre(() => {
    if (!open) return
    void groupId
    untrack(() => {
      const g = group
      draft = { blockedServices: [...(g?.blockedServices ?? [])], schedules: (g?.schedules ?? []).map(copy) }
      editing = null
      err = undefined
    })
  })

  /** Checks the open editor and writes it into the draft; false when it has problems. */
  function applyEditing(): boolean {
    const e = editing
    if (!e) return true
    e.touched = true
    e.serverError = undefined
    if (Object.keys(scheduleProblems(e.draft)).length > 0) return false
    const s = { ...copy(e.draft), name: e.draft.name.trim(), days: normalizeDays(e.draft.days) }
    if (s.block === 'all') s.services = []
    if (e.isNew) draft.schedules.push(s)
    else draft.schedules[e.index] = s
    editing = null
    return true
  }

  function edit(i: number) {
    if (editing?.index === i || !applyEditing()) return
    editing = { index: i, draft: copy(draft.schedules[i]), isNew: false, touched: false }
  }

  function add() {
    if (!applyEditing()) return
    editing = { index: draft.schedules.length, draft: blankSchedule(), isNew: true, touched: false }
  }

  function remove(i: number) {
    if (editing) {
      if (editing.index === i) editing = null
      else if (editing.index > i) editing.index--
    }
    draft.schedules.splice(i, 1)
  }

  const scheduleFields: readonly FieldName[] = ['name', 'days', 'start', 'end', 'block', 'services']

  async function save() {
    err = undefined
    if (!applyEditing()) return
    const g = group
    if (!g) return
    saving = true
    try {
      const saved = await api.parental.update(g.groupId, $state.snapshot(draft))
      toast.success(t('dns.parental.savedToast', { group: saved.groupName }))
      open = false
      onsaved?.(saved)
    } catch (e) {
      const ae = toApiError(e)
      // "schedules[2].start": open that schedule with the message at its field.
      const m = /^schedules\[(\d+)\]\.(\w+)/.exec(ae.field ?? '')
      const i = m ? Number(m[1]) : -1
      const f = m?.[2] as FieldName | undefined
      if (m && i < draft.schedules.length && f && scheduleFields.includes(f)) {
        editing = { index: i, draft: copy(draft.schedules[i]), isNew: false, touched: false, serverError: { field: f, message: errorText(ae) } }
      } else {
        err = ae
      }
    } finally {
      saving = false
    }
  }

  const servicesError = $derived(fieldError(err, 'blockedServices'))
  const schedulesError = $derived(err?.field === 'schedules' || err?.field?.startsWith('schedules[') ? errorText(err) : undefined)
  const generalError = $derived(err && !servicesError && !schedulesError ? errorText(err) : undefined)
  const full = $derived(draft.schedules.length >= MAX_SCHEDULES)
  const subtitle = $derived(
    group
      ? group.groupId === DEFAULT_GROUP_ID
        ? t('dns.parental.defaultNote')
        : tn('dns.parental.devices', group.clientCount)
      : undefined,
  )
</script>

<!-- The open schedule editor (in place of its row, or below the list for a new one). -->
{#snippet editor(e: Editing)}
  <ScheduleEditor
    bind:draft={() => e.draft, (v) => editing && (editing.draft = v)}
    touched={e.touched}
    serverError={e.serverError}
    isNew={e.isNew}
    {catalog}
    {catalogError}
    onretry={onretrycatalog}
    ondone={applyEditing}
    oncancel={() => (editing = null)}
  />
{/snippet}

<FormPanel
  bind:open
  size="lg"
  title={group ? t('dns.parental.editTitle', { group: group.groupName }) : ''}
  {subtitle}
  submitLabel={t('common.action.save')}
  {saving}
  error={generalError}
  onsubmit={save}
>
  <section class="stack-sm" aria-labelledby="pc-always">
    <h3 id="pc-always">{t('dns.parental.editAlwaysTitle')}</h3>
    <p class="small muted">{t('dns.parental.editAlwaysHelp')}</p>
    <ServicePicker
      {catalog}
      {catalogError}
      onretry={onretrycatalog}
      bind:value={draft.blockedServices}
      label={t('dns.parental.editAlwaysTitle')}
      error={servicesError}
    />
  </section>

  <section class="stack-sm" aria-labelledby="pc-schedules">
    <h3 id="pc-schedules">{t('dns.parental.editSchedulesTitle')}</h3>
    <p class="small muted">{t('dns.parental.editSchedulesHelp', { max: MAX_SCHEDULES })}</p>
    {#if schedulesError}<Notice tone="fail">{schedulesError}</Notice>{/if}

    {#if draft.schedules.length > 0}
      <ul class="list">
        {#each draft.schedules as s, i (i)}
          <li>
            {#if editing && !editing.isNew && editing.index === i}
              {@render editor(editing)}
            {:else}
              <div class="srow">
                <Toggle bind:checked={s.enabled} ariaLabel={t('dns.parental.enableSchedule', { name: s.name })} />
                <span class="info">
                  <span class="sname">{s.name}</span>
                  <span class="small muted">{daysText(s.days)} · {windowText(s)}</span>
                  <span class="small muted">{t('dns.parental.schedule.blocks', { what: blockText(s, catalog) })}</span>
                </span>
                <span class="btns">
                  <IconButton icon="edit" size="sm" label={t('dns.parental.editSchedule', { name: s.name })} onclick={() => edit(i)} />
                  <IconButton
                    icon="trash"
                    size="sm"
                    variant="danger"
                    label={t('dns.parental.deleteSchedule', { name: s.name })}
                    onclick={() => remove(i)}
                  />
                </span>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {:else if !editing}
      <p class="none small muted">{t('dns.parental.schedules.noneEdit')}</p>
    {/if}

    {#if editing?.isNew}
      {@render editor(editing)}
    {:else}
      <div class="row">
        <Button icon="plus" disabled={full} onclick={add}>{t('dns.parental.addSchedule')}</Button>
        {#if full}<span class="small muted">{t('dns.parental.maxSchedules', { max: MAX_SCHEDULES })}</span>{/if}
      </div>
    {/if}
  </section>
</FormPanel>

<style>
  h3 {
    font-size: var(--fs-md);
  }
  section + section {
    padding-top: var(--sp-4);
    border-top: 1px solid var(--line);
  }
  .list {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .srow {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .info {
    display: flex;
    flex: 1;
    flex-direction: column;
    min-width: 0;
    line-height: 1.35;
  }
  .sname {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .btns {
    display: inline-flex;
    gap: var(--sp-1);
    margin: -4px -4px 0 0;
  }
  .none {
    padding: var(--sp-2) 0;
  }
</style>
