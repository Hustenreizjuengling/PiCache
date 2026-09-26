<!--
  @component
  Edits the restrictions of one group in a side panel: safe search per
  search engine, the category switches (downloaded lists, always on), the
  services that are always blocked and up to 10 schedules (add, edit
  inline, turn on/off, delete). Everything is sent with "Save changes"
  (PUT /parental/groups/{id}, always with all four members); validation
  errors of the server open the schedule they name. The filtering pause at
  the top acts at once.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type CatalogEntry,
    type CategorySwitch,
    type FilterList,
    type GroupControls,
    type ParentalSchedule,
    type ParentalService,
    type SafeSearch,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, IconButton, Notice, toast, Toggle } from '$lib/ui'
  import FormPanel from '../shared/FormPanel.svelte'
  import PauseMenu from '../shared/PauseMenu.svelte'
  import CategorySwitches from './CategorySwitches.svelte'
  import {
    blankSchedule,
    blockText,
    daysText,
    MAX_SCHEDULES,
    normalizeDays,
    safeSearchOff,
    scheduleProblems,
    switchesOf,
    whenText,
    windowText,
  } from './plan'
  import SafeSearchFields from './SafeSearchFields.svelte'
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

  /**
   * The PUT body: the UI always sends all four members. `categories` holds
   * every switch while editing; save sends only the ones changed here.
   */
  interface Draft {
    blockedServices: string[]
    schedules: ParentalSchedule[]
    safeSearch: SafeSearch
    categories: Partial<Record<CategorySwitch, boolean>>
  }

  interface Props {
    open?: boolean
    group: GroupControls | undefined
    catalog: readonly ParentalService[] | undefined
    catalogError?: ApiError
    onretrycatalog?: () => void
    /** The list catalogue and the lists: names and sizes of the category switches. */
    filterCatalog: readonly CatalogEntry[] | undefined
    lists: readonly FilterList[] | undefined
    onsaved?: (g: GroupControls) => void
    /** The pause changed (it applies at once, without Save). */
    onpaused: (g: GroupControls) => void
    onresume: (g: GroupControls) => void
    resuming?: boolean
  }

  let {
    open = $bindable(false),
    group,
    catalog,
    catalogError,
    onretrycatalog,
    filterCatalog,
    lists,
    onsaved,
    onpaused,
    onresume,
    resuming = false,
  }: Props = $props()

  let draft = $state<Draft>({ blockedServices: [], schedules: [], safeSearch: safeSearchOff(), categories: {} })
  /** The switch states the panel opened with. */
  let openedCategories: Partial<Record<CategorySwitch, boolean>> = {}
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
      openedCategories = g ? Object.fromEntries(switchesOf(g).map((s) => [s.id, !!g.categories[s.id]?.on])) : {}
      draft = {
        blockedServices: [...(g?.blockedServices ?? [])],
        schedules: (g?.schedules ?? []).map(copy),
        safeSearch: { ...safeSearchOff(), ...g?.safeSearch },
        categories: { ...openedCategories },
      }
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
      const body = $state.snapshot(draft)
      // Only the switches changed here. "Off" removes the group from the bound
      // lists, so an untouched switch that shows off because its list is
      // disabled must not be sent: the group keeps that list for later.
      body.categories = Object.fromEntries(
        Object.entries(body.categories).filter(([id, on]) => on !== openedCategories[id as CategorySwitch]),
      )
      const saved = await api.parental.update(g.groupId, body)
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
  const safeSearchError = $derived(fieldError(err, 'safeSearch'))
  const generalError = $derived(err && !servicesError && !schedulesError && !safeSearchError ? errorText(err) : undefined)
  const paused = $derived(!!group?.state.paused)
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
  {#snippet header()}
    {#if group}
      <section class={['pause', paused && 'is-paused']} aria-labelledby="pc-pause">
        <div class="pause-text">
          <h3 id="pc-pause">{t('dns.pause.sectionTitle')}</h3>
          <p class="small">
            {paused && group.state.pausedUntil
              ? t('dns.pause.pausedUntil', { when: whenText(group.state.pausedUntil, 'until') })
              : t('dns.pause.active')}
          </p>
          <p class="small muted">{t('dns.pause.keeps')}</p>
        </div>
        <div class="pause-actions">
          <PauseMenu
            groupId={group.groupId}
            groupName={group.groupName}
            clientCount={group.clientCount}
            disabled={!group.groupEnabled || saving}
            {onpaused}
          />
          {#if paused}
            <Button size="sm" icon="play" loading={resuming} onclick={() => group && onresume(group)}>{t('dns.pause.resume')}</Button>
          {/if}
        </div>
      </section>
    {/if}
  {/snippet}

  <section class="stack-sm" aria-labelledby="pc-safe">
    <h3 id="pc-safe">{t('dns.parental.safeSearch.title')}</h3>
    <p class="small muted">{t('dns.parental.safeSearch.editHelp')}</p>
    <SafeSearchFields bind:value={draft.safeSearch} youtubeError={safeSearchError} />
  </section>

  {#if group && switchesOf(group).length > 0}
    <section class="stack-sm" aria-labelledby="pc-cats">
      <h3 id="pc-cats">{t('dns.parental.switch.title')}</h3>
      <p class="small muted">{t('dns.parental.switch.editHelp')}</p>
      <CategorySwitches bind:value={draft.categories} {group} {filterCatalog} {lists} />
      <p class="small muted">{t('dns.parental.switch.enableNote')}</p>
    </section>
  {/if}

  <section class="stack-sm" aria-labelledby="pc-always">
    <h3 id="pc-always">{t('dns.parental.editAlwaysTitle')}</h3>
    <p class="small muted">{t('dns.parental.editAlwaysHelp')} {t('dns.parental.services.vsSwitches')}</p>
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
  /* The pause acts at once: it sits above the form, apart from what Save sends. */
  .pause {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-3);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .is-paused {
    border-color: color-mix(in srgb, var(--warn) 45%, var(--surface));
  }
  .pause-text {
    display: flex;
    flex: 1 1 260px;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .pause-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
</style>
