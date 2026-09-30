<!--
  @component
  Multi-select for PiCache's DNSSEC verdicts (secure, insecure, bogus,
  indeterminate): a button that opens a popover with one checkbox per
  status. Changes apply immediately; several statuses match any of them.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DnssecStatus } from '$lib/api'
  import { Checkbox, getFieldContext, Icon } from '$lib/ui'
  import { placeMenu } from '$lib/ui/position'
  import { DNSSEC_STATUSES, dnssecStatusLabel, dnssecStatusTone } from '../shared/dnssec'

  interface Props {
    value: DnssecStatus[]
    onchange: (value: DnssecStatus[]) => void
    id?: string
  }

  let { value, onchange, id }: Props = $props()

  const field = getFieldContext()
  const auto = $props.id()
  const popId = `dnssec-pop-${auto}`
  let btn: HTMLButtonElement
  let pop: HTMLDivElement
  let open = $state(false)

  const summary = $derived.by(() => {
    if (value.length === 0) return t('dns.queryLog.dnssecStatus.all')
    if (value.length === 1) return dnssecStatusLabel(value[0])
    return t('dns.queryLog.dnssecStatus.count', { count: value.length })
  })

  function toggle(s: DnssecStatus, on: boolean) {
    const next = on ? [...value, s] : value.filter((x) => x !== s)
    onchange(DNSSEC_STATUSES.filter((x) => next.includes(x)))
  }

  const SWATCH = { ok: 'var(--ok)', warn: 'var(--warn)', fail: 'var(--fail)' } as Record<string, string>

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
  aria-label="{t('dns.queryLog.dnssecStatus.label')}: {summary}"
  popovertarget={popId}
  aria-haspopup="dialog"
  aria-expanded={open}
>
  <span class="lbl">{summary}</span>
  <Icon name="chevron-down" size={16} />
</button>

<div bind:this={pop} id={popId} popover="auto" class="pop" role="dialog" aria-label={t('dns.queryLog.dnssecStatus.label')}>
  {#if value.length > 0}
    <div class="quick">
      <button type="button" class="link" onclick={() => onchange([])}>{t('dns.queryLog.dnssecStatus.all')}</button>
    </div>
  {/if}
  <div class="list">
    {#each DNSSEC_STATUSES as s (s)}
      <span class="opt">
        <span class="swatch" style:--c={SWATCH[dnssecStatusTone(s)] ?? 'var(--text-3)'} aria-hidden="true"></span>
        <Checkbox checked={value.includes(s)} label={dnssecStatusLabel(s)} onchange={(on) => toggle(s, on)} />
      </span>
    {/each}
  </div>
  <p class="note small muted">{t('dns.queryLog.dnssecStatus.note')}</p>
</div>

<style>
  .trigger {
    display: inline-flex;
    align-items: center;
    justify-content: space-between;
    gap: 6px;
    width: 100%;
    min-width: 120px;
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
    min-width: 200px;
    max-width: min(300px, calc(100vw - 2 * var(--sp-4)));
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
  .note {
    line-height: 1.4;
  }
</style>
