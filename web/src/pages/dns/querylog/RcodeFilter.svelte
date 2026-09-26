<!--
  @component
  Multi-select for reply codes: a button that opens a popover with one
  checkbox per code (the common ones, the codes of the loaded page and the
  selected ones). Changes apply immediately; several codes match any of them.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { Checkbox, getFieldContext, Icon } from '$lib/ui'
  import { placeMenu } from '$lib/ui/position'
  import { MAX_RCODES, RCODES } from './filters'

  interface Props {
    value: string[]
    /** Codes seen in the loaded page (offered besides the common ones). */
    seen?: readonly string[]
    onchange: (value: string[]) => void
    id?: string
  }

  let { value, seen = [], onchange, id }: Props = $props()

  const field = getFieldContext()
  const auto = $props.id()
  const popId = `rcode-pop-${auto}`
  let btn: HTMLButtonElement
  let pop: HTMLDivElement
  let open = $state(false)

  const codes = $derived([...new Set([...RCODES, ...seen.map((c) => c.toUpperCase()), ...value])].filter(Boolean))

  const summary = $derived.by(() => {
    if (value.length === 0) return t('dns.queryLog.rcode.all')
    if (value.length === 1) return value[0]
    return t('dns.queryLog.rcode.count', { count: value.length })
  })

  function toggle(c: string, on: boolean) {
    const next = on ? [...value, c] : value.filter((x) => x !== c)
    onchange(codes.filter((x) => next.includes(x)).slice(0, MAX_RCODES))
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
  aria-label="{t('dns.queryLog.rcode.label')}: {summary}"
  popovertarget={popId}
  aria-haspopup="dialog"
  aria-expanded={open}
>
  <span class="lbl">{summary}</span>
  <Icon name="chevron-down" size={16} />
</button>

<div bind:this={pop} id={popId} popover="auto" class="pop" role="dialog" aria-label={t('dns.queryLog.rcode.label')}>
  {#if value.length > 0}
    <div class="quick">
      <button type="button" class="link" onclick={() => onchange([])}>{t('dns.queryLog.rcode.all')}</button>
    </div>
  {/if}
  <div class="list">
    {#each codes as c (c)}
      <Checkbox checked={value.includes(c)} label={c} onchange={(on) => toggle(c, on)} />
    {/each}
  </div>
  <p class="note small muted">{t('dns.queryLog.rcode.note')}</p>
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
    font-family: var(--font-mono);
  }
  .note {
    line-height: 1.4;
  }
</style>
