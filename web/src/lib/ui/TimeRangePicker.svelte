<!--
  @component
  Segmented control for chart and list ranges (independent of retention).
  <TimeRangePicker bind:value={range} onchange={(r) => router.setQuery({ range: r })} />
  Longer presets can sit in a "More" menu (`more`), which also offers
  "Custom range…" when `oncustom` is set (the page opens CustomRangeDialog);
  a custom window is shown with `custom` (no segment is selected then).
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import type { RangePreset } from '../api/types'
  import { formatSpan } from '../format'
  import type { CustomRange } from '../range'
  import Icon from './Icon.svelte'
  import Menu from './Menu.svelte'
  import type { MenuItem } from './types'

  interface Props {
    /** null: no preset is selected (the page shows an explicit time window instead). */
    value?: RangePreset | null
    options?: RangePreset[]
    /** Longer presets offered under "More". */
    more?: RangePreset[]
    /** The custom window shown instead of a preset. */
    custom?: CustomRange | null
    /** Offers "Custom range…" under "More". */
    oncustom?: () => void
    /** Accessible name (default "Time range"). */
    label?: string
    onchange?: (value: RangePreset) => void
  }

  let {
    value = $bindable('24h'),
    options = ['15m', '1h', '24h', '7d', '30d'],
    more = [],
    custom = null,
    oncustom,
    label,
    onchange,
  }: Props = $props()

  let group: HTMLDivElement
  /** Index of the selected option (-1: none, e.g. an explicit window or a preset under "More"). */
  const selected = $derived(value && !custom ? options.indexOf(value) : -1)

  function select(v: RangePreset) {
    if (v === value && !custom) return
    value = v
    onchange?.(v)
  }

  function onKey(e: KeyboardEvent) {
    const i = selected
    let n = -1
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') n = (i + 1) % options.length
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') n = i < 0 ? options.length - 1 : (i - 1 + options.length) % options.length
    if (n < 0) return
    e.preventDefault()
    select(options[n])
    group.querySelectorAll<HTMLElement>('[role="radio"]')[n]?.focus()
  }

  /** A preset of the "More" menu is selected. */
  const moreValue = $derived(!custom && value && more.includes(value) ? value : null)
  const moreActive = $derived(!!moreValue || !!custom)
  const moreText = $derived(
    custom ? formatSpan(custom.from * 1000, custom.to * 1000) : moreValue ? t(`common.range.short.${moreValue}`) : t('common.range.more'),
  )
  /** Accessible name of the "More" button: the selected range it shows first (no segment is checked then). */
  const moreName = $derived(moreActive ? t('common.range.moreSelected', { range: moreText }) : t('common.range.moreLabel'))

  const items = $derived.by((): MenuItem[] => {
    const list: MenuItem[] = more.map((p) => ({
      label: t(`common.range.long.${p}`),
      checked: !custom && value === p,
      onselect: () => select(p),
    }))
    if (oncustom) {
      if (list.length > 0) list.push({ separator: true })
      list.push({ label: t('common.range.custom'), checked: !!custom, onselect: oncustom })
    }
    return list
  })
</script>

<div class="picker">
  <div bind:this={group} class="seg" role="radiogroup" aria-label={label ?? t('common.range.label')} tabindex="-1" onkeydown={onKey}>
    {#each options as o, i (o)}
      <button
        type="button"
        role="radio"
        aria-checked={o === value && !custom}
        tabindex={i === Math.max(0, selected) ? 0 : -1}
        title={t(`common.range.long.${o}`)}
        onclick={() => select(o)}>{t(`common.range.short.${o}`)}</button
      >
    {/each}
  </div>
  {#if items.length > 0}
    <span class={['more', moreActive && 'active']}>
      <Menu label={moreName} size="sm" items={items}>
        {#snippet trigger()}
          <span class="more-text">{moreText}</span><Icon name="chevron-down" size={16} />
        {/snippet}
      </Menu>
    </span>
  {/if}
</div>

<style>
  .picker {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1);
    max-width: 100%;
  }
  .seg {
    display: inline-flex;
    padding: 2px;
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
  }
  button {
    height: calc(var(--control-h-sm) - 2px);
    min-width: 44px;
    padding: 0 var(--sp-2);
    border: 0;
    border-radius: 4px;
    background: none;
    color: var(--text-2);
    font-size: var(--fs-sm);
    font-weight: 600;
    cursor: pointer;
  }
  button:hover {
    color: var(--text);
  }
  button[aria-checked='true'] {
    background: var(--text);
    color: var(--surface);
  }
  .more {
    display: inline-flex;
    min-width: 0;
  }
  /* As tall as the segmented control next to it. */
  .more :global(.trigger) {
    height: calc(var(--control-h-sm) + 4px);
    min-width: 0;
    max-width: 100%;
    border-color: var(--line-strong);
    color: var(--text-2);
    font-size: var(--fs-sm);
  }
  .more.active :global(.trigger),
  .more.active :global(.trigger:hover) {
    border-color: var(--text);
    background: var(--text);
    color: var(--surface);
  }
  .more-text {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
