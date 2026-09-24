<!--
  @component
  Tab list with one panel (ARIA tabs, arrow keys switch tabs). Keep the
  active tab in the URL with router.setQuery({ tab }) so it survives reloads.
  <Tabs label="Filtering" tabs={[{ id: 'lists', label: 'Blocklists', count: 3 }, { id: 'rules', label: 'Rules' }]}
        bind:active={tab}>
    {#snippet children(active)}
      {#if active === 'lists'}<Lists />{:else}<Rules />{/if}
    {/snippet}
  </Tabs>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import Badge from './Badge.svelte'
  import Icon from './Icon.svelte'
  import type { TabItem } from './types'

  interface Props {
    tabs: TabItem[]
    active?: string
    /** Accessible name of the tab list. */
    label: string
    onchange?: (id: string) => void
    children?: Snippet<[string]>
  }

  let { tabs, active = $bindable(tabs[0]?.id ?? ''), label, onchange, children }: Props = $props()

  const auto = $props.id()
  const base = `tabs-${auto}`
  let list: HTMLDivElement

  function select(id: string) {
    if (id === active) return
    active = id
    onchange?.(id)
  }

  function onKey(e: KeyboardEvent) {
    const i = tabs.findIndex((x) => x.id === active)
    let next = -1
    if (e.key === 'ArrowRight') next = (i + 1) % tabs.length
    else if (e.key === 'ArrowLeft') next = (i - 1 + tabs.length) % tabs.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = tabs.length - 1
    if (next < 0) return
    e.preventDefault()
    select(tabs[next].id)
    list.querySelector<HTMLElement>(`#${base}-${CSS.escape(tabs[next].id)}`)?.focus()
  }
</script>

<div class="tabs">
  <div bind:this={list} class="list" role="tablist" aria-label={label} tabindex="-1" onkeydown={onKey}>
    {#each tabs as tab (tab.id)}
      <button
        type="button"
        role="tab"
        id="{base}-{tab.id}"
        class="tab"
        aria-selected={tab.id === active}
        aria-controls="{base}-panel"
        tabindex={tab.id === active ? 0 : -1}
        onclick={() => select(tab.id)}
      >
        {#if tab.icon}<Icon name={tab.icon} size={18} />{/if}
        <span>{tab.label}</span>
        {#if tab.count !== undefined}<Badge>{tab.count}</Badge>{/if}
      </button>
    {/each}
  </div>
  {#if children}
    <div class="panel" role="tabpanel" id="{base}-panel" aria-labelledby="{base}-{active}">
      {@render children(active)}
    </div>
  {/if}
</div>

<style>
  .tabs {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    min-width: 0;
  }
  .list {
    display: flex;
    gap: var(--sp-1);
    border-bottom: 1px solid var(--line);
    overflow-x: auto;
    scrollbar-width: thin;
  }
  .tab {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    height: 40px;
    padding: 0 var(--sp-3);
    border: 0;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
    background: none;
    color: var(--text-2);
    font-weight: 600;
    white-space: nowrap;
    cursor: pointer;
  }
  .tab:hover {
    color: var(--text);
  }
  .tab[aria-selected='true'] {
    color: var(--text);
    border-bottom-color: var(--text);
  }
  .tab:focus-visible {
    outline-offset: -2px;
  }
  .panel {
    min-width: 0;
  }
</style>
