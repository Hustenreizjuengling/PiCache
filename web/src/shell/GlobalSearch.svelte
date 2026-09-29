<!--
  @component
  Header search, an ARIA combobox: from 2 characters the settings that
  match (lib/searchIndex.ts; matched in the browser, the text never reaches
  the server), then one fixed option: "Open client <ip>" for an IP address,
  else "Search the query log for <q>" (from 3 characters). ↑/↓ move, Enter
  opens the active option, Esc closes the list, Tab leaves. Enter without an
  active option opens the client of an IP address (#/dns/clients?ip=…) or
  searches the query log (#/dns/queries?domain=…). A settings hit opens its
  page with ?jump= (and &field=): the shell scrolls there and focuses it.
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import type { IconName } from '../lib/icons'
  import { router } from '../lib/router.svelte'
  import { highlight, MIN_QUERY, searchSettings, type SearchHit } from '../lib/searchIndex'
  import { Icon, Input, toast } from '../lib/ui'
  import { resolve } from '../routes'

  let value = $state('')
  let focused = $state(false)
  /** Esc closed the list; typing or ↓ opens it again. */
  let dismissed = $state(false)
  /** Index of the active option (-1: none). */
  let active = $state(-1)

  const auto = $props.id()
  const listId = `search-${auto}-list`
  const optionId = (i: number) => `search-${auto}-opt-${i}`

  function isIPv4(s: string): boolean {
    const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s)
    return !!m && m.slice(1).every((p) => Number(p) <= 255)
  }

  function isIPv6(s: string): boolean {
    if (!s.includes(':') || !/^[0-9a-f:.]+$/i.test(s)) return false
    try {
      new URL(`http://[${s}]/`)
      return true
    } catch {
      return false
    }
  }

  type Option =
    | { kind: 'setting'; hit: SearchHit; page: string }
    | { kind: 'client' | 'log'; label: string; icon: IconName }

  const query = $derived(value.trim())
  /** What the fixed options search for: lower case, without a trailing dot. */
  const target = $derived(query.toLowerCase().replace(/\.$/, ''))
  const isIp = $derived(isIPv4(target) || isIPv6(target))

  function pageTitle(route: string): string {
    const r = resolve(route)
    return r ? t(r.route.title) : route
  }

  const options = $derived.by((): Option[] => {
    if (query.length < MIN_QUERY) return []
    const out: Option[] = searchSettings(query).map((hit) => ({ kind: 'setting', hit, page: pageTitle(hit.entry.route) }))
    if (isIp) out.push({ kind: 'client', label: t('common.search.openClient', { ip: target }), icon: 'user' })
    else if (target.length >= 3) out.push({ kind: 'log', label: t('common.search.searchLog', { query: target }), icon: 'list' })
    return out
  })

  const open = $derived(focused && !dismissed && options.length > 0)

  function choose(o: Option) {
    if (o.kind === 'setting') {
      const e = o.hit.entry
      router.navigate(e.route, { jump: e.anchor, field: e.field })
    } else if (o.kind === 'client') {
      router.navigate('/dns/clients', { ip: target })
    } else {
      router.navigate('/dns/queries', { domain: target })
    }
    value = ''
    active = -1
  }

  function oninput() {
    active = -1
    dismissed = false
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.isComposing) return
    const n = options.length
    switch (e.key) {
      case 'ArrowDown':
        if (n === 0) return
        e.preventDefault()
        dismissed = false
        active = (active + 1) % n
        break
      case 'ArrowUp':
        if (n === 0) return
        e.preventDefault()
        dismissed = false
        active = active <= 0 ? n - 1 : active - 1
        break
      case 'Enter':
        if (open && active >= 0 && active < n) {
          e.preventDefault()
          choose(options[active])
        }
        break
      case 'Escape':
        if (open) {
          e.preventDefault()
          dismissed = true
          active = -1
        }
        break
      case 'Tab':
        dismissed = true
        break
    }
  }

  // Enter without an active option: a client for an IP address, else the query log.
  function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!target) return
    if (isIp) {
      router.navigate('/dns/clients', { ip: target })
    } else if (target.length < 3) {
      toast.info(t('common.search.tooShort'))
      return
    } else {
      router.navigate('/dns/queries', { domain: target })
    }
    value = ''
    active = -1
  }
</script>

<form class="search" role="search" onsubmit={submit}>
  <Input
    type="search"
    icon="search"
    size="sm"
    bind:value
    maxlength={253}
    placeholder={t('common.search.placeholder')}
    aria-label={t('common.search.label')}
    autocomplete="off"
    role="combobox"
    aria-autocomplete="list"
    aria-expanded={open}
    aria-controls={listId}
    aria-activedescendant={open && active >= 0 ? optionId(active) : undefined}
    {oninput}
    {onkeydown}
    onfocus={() => (focused = true)}
    onblur={() => {
      focused = false
      active = -1
    }}
  />
  <ul class="list" id={listId} role="listbox" aria-label={t('common.search.results')} hidden={!open}>
    {#each options as o, i (o.kind === 'setting' ? `${o.hit.entry.anchor}#${o.hit.entry.field ?? ''}` : o.kind)}
      <!-- The input keeps the focus (aria-activedescendant): a press picks the option without taking it. -->
      <li
        id={optionId(i)}
        role="option"
        tabindex="-1"
        aria-selected={i === active}
        class={['opt', i === active && 'active', o.kind !== 'setting' && 'fixed']}
        onmousedown={(e) => {
          e.preventDefault()
          if (e.button === 0) choose(o)
        }}
      >
        {#if o.kind === 'setting'}
          <Icon name="sliders" size={16} />
          <span class="text">
            <span class="title">
              {#each highlight(o.hit.title, query) as p, j (j)}{#if p.hit}<mark>{p.text}</mark>{:else}{p.text}{/if}{/each}
            </span>
            <span class="where">{o.hit.context ? `${o.page} › ${o.hit.context}` : o.page}</span>
          </span>
        {:else}
          <Icon name={o.icon} size={16} />
          <span class="text"><span class="title">{o.label}</span></span>
        {/if}
      </li>
    {/each}
  </ul>
</form>

<style>
  .search {
    position: relative;
    width: 240px;
    max-width: 100%;
  }
  /* Anchored at the search box's left edge: the header's tools follow it on
     the right (theme, language, account), so a list wider than the box
     stays on the page also where the tools wrap to the left edge of the
     header (below 900 px, without the sidebar). */
  .list {
    position: absolute;
    top: calc(100% + 4px);
    left: 0;
    z-index: 30;
    width: max(100%, 380px);
    max-width: calc(100vw - 2 * var(--sp-4));
    max-height: min(440px, 70vh);
    margin: 0;
    padding: var(--sp-1);
    overflow-y: auto;
    list-style: none;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    box-shadow: var(--shadow-float);
  }
  .list[hidden] {
    display: none;
  }
  .opt {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-2);
    border-radius: 4px;
    color: var(--text);
    cursor: pointer;
  }
  .opt :global(.icon) {
    flex: none;
    margin-top: 2px;
    color: var(--text-3);
  }
  .opt.fixed {
    border-top: 1px solid var(--line);
    border-radius: 0 0 4px 4px;
  }
  .opt.fixed:first-child {
    border-top: 0;
    border-radius: 4px;
  }
  .opt:hover,
  .opt.active {
    background: var(--surface-2);
  }
  .opt.active {
    box-shadow: inset 2px 0 0 var(--focus);
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .title {
    font-size: var(--fs-sm);
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .where {
    color: var(--text-2);
    font-size: var(--fs-xs);
    overflow-wrap: anywhere;
  }
  mark {
    border-radius: 2px;
    background: var(--highlight);
    color: inherit;
  }
  @media (max-width: 640px) {
    /* As wide as the header (the positioned top bar), never wider than the page. */
    .search {
      position: static;
      width: 100%;
    }
    .list {
      top: calc(100% - 1px);
      left: var(--sp-2);
      right: var(--sp-2);
      width: auto;
      max-width: none;
    }
  }
</style>
