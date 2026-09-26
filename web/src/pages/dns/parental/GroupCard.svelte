<!--
  @component
  One group on the parental controls page: name, devices, what applies right
  now in plain words, the weekly plan, its schedules, the services that are
  always blocked, safe search and the category switches (with download
  warnings), and a badge while its filtering is paused. Admins get the quick
  actions (block internet now, lift restrictions, end an override, pause
  filtering, resume) and Edit; read-only principals see the same without
  actions.
-->
<script lang="ts">
  import { t, tn, type MessageKey } from '$i18n/index.svelte'
  import { DEFAULT_GROUP_ID, type GroupControls, type OverrideMode, type ParentalService } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Menu, type MenuItem } from '$lib/ui'
  import PauseMenu from '../shared/PauseMenu.svelte'
  import {
    blockText,
    daysText,
    hasRestrictions,
    QUICK_MINUTES,
    safeSearchNames,
    serviceNames,
    stateText,
    switchesOf,
    whenText,
    windowText,
  } from './plan'
  import WeekPlan from './WeekPlan.svelte'

  interface Props {
    group: GroupControls
    catalog: readonly ParentalService[] | undefined
    now: Date
    /** An override or pause request of this group is running. */
    busy?: boolean
    onedit: (g: GroupControls) => void
    /** minutes, or 'until' to ask for a time. */
    onoverride: (g: GroupControls, mode: OverrideMode, minutes: number | 'until') => void
    onend: (g: GroupControls) => void
    onpaused: (g: GroupControls) => void
    onresume: (g: GroupControls) => void
  }

  let { group, catalog, now, busy = false, onedit, onoverride, onend, onpaused, onresume }: Props = $props()

  const auto = $props.id()
  const isDefault = $derived(group.groupId === DEFAULT_GROUP_ID)
  const state = $derived(stateText(group, catalog, now))
  const schedules = $derived(group.schedules ?? [])
  const always = $derived(serviceNames(group.blockedServices ?? [], catalog))
  const enabledSchedules = $derived(schedules.filter((s) => s.enabled))
  const restricted = $derived(hasRestrictions(group))
  const safe = $derived(safeSearchNames(group.safeSearch))
  const switches = $derived(switchesOf(group).filter((s) => group.categories[s.id]?.on))
  const paused = $derived(group.state.paused && !!group.state.pausedUntil)

  function durationLabel(m: number): string {
    return m < 60 ? t('dns.parental.forMinutes', { count: m }) : tn('dns.parental.forHours', m / 60)
  }

  function items(mode: OverrideMode): MenuItem[] {
    return [
      ...QUICK_MINUTES.map((m) => ({ label: durationLabel(m), onselect: () => onoverride(group, mode, m) })),
      { separator: true as const },
      { label: t('dns.parental.untilTime'), icon: 'clock' as const, onselect: () => onoverride(group, mode, 'until') },
      // Content protection is not lifted by the allow override.
      ...(mode === 'allow' ? [{ separator: true as const }, { note: t('dns.parental.liftKeeps') }] : []),
    ]
  }
</script>

<section class="card" aria-labelledby="pc-{auto}">
  <header>
    <div class="titles">
      <div class="name-row">
        <h2 id="pc-{auto}">{group.groupName}</h2>
        {#if isDefault}<Badge tone="info">{t('dns.groups.default')}</Badge>{/if}
        {#if !group.groupEnabled}<Badge tone="warn">{t('dns.parental.groupDisabled')}</Badge>{/if}
        {#if paused && group.state.pausedUntil}
          <Badge tone="warn" title={t('dns.pause.keeps')}>{t('dns.pause.badge', { when: whenText(group.state.pausedUntil, 'until', now) })}</Badge>
        {/if}
      </div>
      <p class="meta small muted">
        {#if isDefault}
          {t('dns.parental.defaultNote')}
        {:else if group.clientCount > 0}
          <a href={href('/dns/clients', { group: group.groupId })}>{tn('dns.parental.devices', group.clientCount)}</a>
        {:else}
          {t('dns.parental.noDevices')}
          <a href={href('/dns/clients')}>{t('dns.parental.addDevices')}</a>
        {/if}
      </p>
    </div>
    <!-- Overrides and pauses end by themselves (at most 7 days): admins may use them while the configuration is locked. -->
    {#if session.canOperate}
      <div class="actions">
        <!-- An override of a disabled group would apply to nobody. -->
        <Menu label={t('dns.parental.block')} icon="pause" items={items('block')} disabled={busy || !group.groupEnabled} size="sm" align="end" />
        <Menu
          label={t('dns.parental.lift')}
          icon="shield-off"
          items={items('allow')}
          disabled={busy || !restricted || !group.groupEnabled}
          size="sm"
          align="end"
        />
        {#if paused}
          <Button size="sm" icon="play" loading={busy} onclick={() => onresume(group)}>{t('dns.pause.resume')}</Button>
        {:else}
          <PauseMenu
            groupId={group.groupId}
            groupName={group.groupName}
            clientCount={group.clientCount}
            disabled={busy || !group.groupEnabled}
            {onpaused}
          />
        {/if}
        {#if session.isAdmin}
          <Button size="sm" icon="edit" onclick={() => onedit(group)}>{t('dns.parental.edit')}</Button>
        {/if}
      </div>
    {/if}
  </header>

  <div class={['state', `is-${state.kind}`]}>
    <span class="mark" aria-hidden="true"></span>
    <div class="state-text">
      <p class="sentence">{state.text}</p>
      {#if state.next}<p class="small muted">{state.next}</p>{/if}
      {#if !group.groupEnabled}
        <p class="small muted">
          {t('dns.parental.disabledText')}
          <a href={href('/dns/clients', { tab: 'groups', sel: group.groupId })}>{t('dns.parental.openGroup')}</a>
        </p>
      {/if}
    </div>
    {#if group.override && session.canOperate}
      <Button size="sm" variant="ghost" icon="close" loading={busy} onclick={() => onend(group)}>
        {t('dns.parental.end')}
      </Button>
    {/if}
  </div>

  <!-- Without a schedule in use there is no plan to draw: the summary takes the width. -->
  <div class={['body', enabledSchedules.length === 0 && 'no-plan']}>
    {#if enabledSchedules.length > 0}
      <div class="plan">
        <h3 class="visually-hidden">{t('dns.parental.plan.title')}</h3>
        <WeekPlan
          {schedules}
          {catalog}
          {now}
          label={t('dns.parental.plan.label', { group: group.groupName })}
          faded={!group.groupEnabled || group.state.lifted}
        />
      </div>
    {/if}

    <div class="summary">
      <section class="stack-sm" aria-labelledby="pc-{auto}-sched">
        <h3 id="pc-{auto}-sched">{t('dns.parental.schedules.title')}</h3>
        {#if schedules.length === 0}
          <p class="small muted">{t('dns.parental.schedules.none')}</p>
        {:else if enabledSchedules.length === 0}
          <p class="small muted">{t('dns.parental.plan.none')}</p>
        {/if}
        {#if schedules.length > 0}
          <ul class="schedules">
            {#each schedules as s (s.id || s.name)}
              <li class={{ off: !s.enabled }}>
                <span class={['swatch', s.block]} aria-hidden="true"></span>
                <span class="sched">
                  <span class="sname">
                    {s.name}
                    {#if !s.enabled}<Badge>{t('dns.parental.schedule.off')}</Badge>{/if}
                  </span>
                  <span class="small muted">{daysText(s.days)} · {windowText(s)}</span>
                  <span class="small muted">{t('dns.parental.schedule.blocks', { what: blockText(s, catalog) })}</span>
                </span>
              </li>
            {/each}
          </ul>
        {/if}
      </section>
      <section class="stack-sm" aria-labelledby="pc-{auto}-always">
        <h3 id="pc-{auto}-always">{t('dns.parental.always.title')}</h3>
        {#if always.length === 0}
          <p class="small muted">{t('dns.parental.always.none')}</p>
        {:else}
          <ul class="always">
            {#each always as name (name)}<li>{name}</li>{/each}
          </ul>
        {/if}
      </section>
      <section class="stack-sm" aria-labelledby="pc-{auto}-safe">
        <h3 id="pc-{auto}-safe">{t('dns.parental.safeSearch.title')}</h3>
        {#if safe.length === 0}
          <p class="small muted">{t('dns.parental.safeSearch.none')}</p>
        {:else}
          <ul class="always">
            {#each safe as name (name)}<li>{name}</li>{/each}
          </ul>
        {/if}
      </section>
      {#if switchesOf(group).length > 0}
        <section class="stack-sm" aria-labelledby="pc-{auto}-cats">
          <h3 id="pc-{auto}-cats">{t('dns.parental.switch.cardTitle')}</h3>
          {#if switches.length === 0}
            <p class="small muted">{t('dns.parental.switch.none')}</p>
          {:else}
            <ul class="always">
              {#each switches as s (s.id)}
                {@const st = group.categories[s.id]?.state}
                <li class={{ warn: st === 'pending', fail: st === 'failed' }}>
                  {t(`dns.parental.switch.${s.id}` as MessageKey)}
                  {#if st === 'pending'}
                    <Badge tone="warn" title={t('dns.parental.switch.pending')}>{t('dns.parental.switch.pendingShort')}</Badge>
                  {:else if st === 'failed'}
                    <Badge tone="fail" title={t('dns.parental.switch.failed')}>{t('dns.parental.switch.failedShort')}</Badge>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
        </section>
      {/if}
    </div>
  </div>
</section>

<style>
  .card {
    min-width: 0;
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
  }
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-4);
    padding: var(--sp-4) var(--sp-4) 0;
  }
  .titles {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .name-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
  }
  h2 {
    font-size: var(--fs-lg);
    overflow-wrap: anywhere;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
  /* The state: one sentence with a mark that matches the plan's legend. */
  .state {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    margin: var(--sp-3) var(--sp-4) 0;
    padding: var(--sp-3);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .state-text {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .sentence {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .mark {
    flex: none;
    width: 14px;
    height: 14px;
    margin-top: 4px;
    border-radius: 3px;
    box-shadow: inset 0 0 0 2px var(--text-3);
  }
  .is-blocked .mark {
    background: var(--orange);
    box-shadow: none;
  }
  .is-services .mark {
    background: repeating-linear-gradient(-45deg, var(--orange) 0 3px, color-mix(in srgb, var(--orange) 26%, var(--surface)) 3px 6px);
    box-shadow: inset 0 0 0 1px var(--orange);
  }
  .is-lifted .mark {
    box-shadow: inset 0 0 0 2px var(--focus);
  }
  .is-none .mark {
    box-shadow: inset 0 0 0 2px var(--ok);
  }
  .body {
    display: grid;
    grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);
    gap: var(--sp-4) var(--sp-6);
    padding: var(--sp-4);
  }
  .no-plan {
    grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);
  }
  .no-plan .summary {
    display: contents;
  }
  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    min-width: 0;
  }
  h3 {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .schedules {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .schedules li {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
  }
  .off .sname {
    color: var(--text-2);
  }
  .sched {
    display: flex;
    flex-direction: column;
    min-width: 0;
    line-height: 1.35;
  }
  .sname {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    font-size: var(--fs-sm);
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .swatch {
    flex: none;
    width: 12px;
    height: 12px;
    margin-top: 3px;
    border-radius: 3px;
  }
  .swatch.all {
    background: var(--orange);
  }
  .swatch.services {
    background: repeating-linear-gradient(-45deg, var(--orange) 0 3px, color-mix(in srgb, var(--orange) 26%, var(--surface)) 3px 6px);
    box-shadow: inset 0 0 0 1px var(--orange);
  }
  .off .swatch {
    opacity: 0.4;
  }
  .always {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .always li {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
    padding: 1px var(--sp-2);
    border: 1px solid var(--line);
    border-radius: var(--r-pill);
    background: var(--surface-2);
    font-size: var(--fs-sm);
  }
  .always li.warn {
    border-color: color-mix(in srgb, var(--warn) 50%, var(--surface));
    padding-right: 2px;
  }
  .always li.fail {
    border-color: color-mix(in srgb, var(--fail) 50%, var(--surface));
    padding-right: 2px;
  }
  .always li :global(.badge) {
    height: 18px;
    border-radius: var(--r-pill);
  }
  @media (max-width: 900px) {
    .body,
    .no-plan {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  @media (max-width: 480px) {
    header {
      padding: var(--sp-3) var(--sp-3) 0;
    }
    .state {
      margin: var(--sp-3) var(--sp-3) 0;
    }
    .body {
      padding: var(--sp-3);
    }
  }
</style>
