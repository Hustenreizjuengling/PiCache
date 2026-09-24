<!--
  @component
  Multi-select for query statuses: a button that opens a popover with
  shortcuts (all, blocked, answered) and one checkbox per status. Changes
  apply immediately.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import { BLOCKED_STATUSES, type QueryStatus } from '$lib/api'
  import { queryStatusStyle } from '$lib/traffic'
  import { Checkbox, getFieldContext, Icon } from '$lib/ui'
  import { placeMenu } from '$lib/ui/position'
  import { ALL_STATUSES, ALLOWED_STATUSES } from './filters'

  interface Props {
    value: QueryStatus[]
    onchange: (value: QueryStatus[]) => void
    id?: string
  }

  let { value, onchange, id }: Props = $props()

  const field = getFieldContext()
  const auto = $props.id()
  const popId = `status-pop-${auto}`
  let btn: HTMLButtonElement
  let pop: HTMLDivElement
  let open = $state(false)

  function statusLabel(s: string): string {
    return t(`common.queryStatus.${s}` as MessageKey)
  }

  function sameSet(a: readonly string[], b: readonly string[]): boolean {
    return a.length === b.length && a.every((x) => b.includes(x))
  }

  const summary = $derived.by(() => {
    if (value.length === 0) return t('dns.queryLog.status.all')
    if (sameSet(value, BLOCKED_STATUSES)) return t('dns.queryLog.status.blocked')
    if (sameSet(value, ALLOWED_STATUSES)) return t('dns.queryLog.status.allowed')
    if (value.length === 1) return statusLabel(value[0])
    return t('dns.queryLog.status.count', { count: value.length })
  })

  function set(next: readonly QueryStatus[]) {
    onchange(ALL_STATUSES.filter((s) => next.includes(s)))
  }

  function toggle(s: QueryStatus, on: boolean) {
    set(on ? [...value, s] : value.filter((x) => x !== s))
  }

  $effect(() => {
    const before = (e: Event) => {
      if ((e as ToggleEvent).newState === 'open') placeMenu(btn, pop, 'start')
    }
    const toggled = (e: Event) => {
      open = (e as ToggleEvent).newState === 'open'
      if (open) pop.querySelector<HTMLElement>('button, input')?.focus()
      else if (pop.contains(document.activeElement) || document.activeElement === document.body) btn.focus()
    }
    pop.addEventListener('beforetoggle', before)
    pop.addEventListener('toggle', toggled)
    return () => {
      pop.removeEventListener('beforetoggle', before)
      pop.removeEventListener('toggle', toggled)
    }
  })
</script>

<svelte:window onresize={() => open && placeMenu(btn, pop, 'start')} />

<button
  bind:this={btn}
  id={id ?? field?.id}
  type="button"
  class="trigger"
  aria-label="{t('dns.queryLog.status.label')}: {summary}"
  popovertarget={popId}
  aria-haspopup="dialog"
  aria-expanded={open}
>
  <span class="lbl">{summary}</span>
  <Icon name="chevron-down" size={16} />
</button>

<div bind:this={pop} id={popId} popover="auto" class="pop" role="dialog" aria-label={t('dns.queryLog.status.label')}>
  <div class="quick">
    <button type="button" class="link" onclick={() => set([])}>{t('dns.queryLog.status.all')}</button>
    <button type="button" class="link" onclick={() => set(BLOCKED_STATUSES)}>{t('dns.queryLog.status.blocked')}</button>
    <button type="button" class="link" onclick={() => set(ALLOWED_STATUSES)}>{t('dns.queryLog.status.allowed')}</button>
  </div>
  <div class="list">
    {#each ALL_STATUSES as s (s)}
      {@const style = queryStatusStyle(s)}
      <span class="opt">
        <span
          class={['swatch', style.striped && 'striped']}
          style:--c={style.pair ? `var(--${style.pair})` : style.tone === 'fail' ? 'var(--fail)' : 'var(--warn)'}
          aria-hidden="true"
        ></span>
        <Checkbox checked={value.includes(s)} label={statusLabel(s)} onchange={(on) => toggle(s, on)} />
      </span>
    {/each}
  </div>
</div>

<style>
  .trigger {
    display: inline-flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    width: 100%;
    min-width: 150px;
    height: var(--control-h-sm);
    padding: 0 6px 0 var(--sp-2);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    font-size: var(--fs-sm);
    white-space: nowrap;
    cursor: pointer;
  }
  .trigger:hover {
    background: var(--surface-2);
  }
  .trigger:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
  .lbl {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .pop {
    min-width: 240px;
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-float);
    overflow: auto;
  }
  .pop:popover-open {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
  }
  .quick {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-3);
    padding-bottom: var(--sp-2);
    border-bottom: 1px solid var(--line);
  }
  .link {
    padding: 2px 0;
    border: 0;
    background: none;
    color: var(--focus);
    font: inherit;
    font-size: var(--fs-sm);
    text-decoration: underline;
    cursor: pointer;
  }
  .link:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .list {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .opt {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
  }
  .swatch {
    flex: none;
    width: 10px;
    height: 10px;
    margin-top: 6px;
    border-radius: var(--r-pill);
    background: var(--c);
  }
  .swatch.striped {
    background: repeating-linear-gradient(135deg, var(--c) 0 2px, var(--surface) 2px 4px);
    box-shadow: inset 0 0 0 1px var(--c);
  }
</style>
