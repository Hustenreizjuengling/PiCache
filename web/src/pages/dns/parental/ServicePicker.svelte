<!--
  @component
  Chooses services from the built-in catalogue (about 140): a search box
  (service names) and one collapsible checklist per category with a neutral
  category icon, the number selected and "Select all" / "Clear" for the
  category. Categories with a selection start expanded; while searching,
  every category with a match is. Bound to the selected ids in catalogue
  order. Used for the always-blocked services and for schedules that block
  selected services.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import type { ApiError, ParentalService, ServiceCategory } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { Button, Checkbox, Icon, Input, Skeleton } from '$lib/ui'
  import { CATEGORIES, CATEGORY_ICONS } from './plan'

  interface Props {
    catalog: readonly ParentalService[] | undefined
    catalogError?: ApiError
    onretry?: () => void
    value?: string[]
    /** Accessible name of the whole picker. */
    label: string
    help?: string
    error?: string
    disabled?: boolean
  }

  let { catalog, catalogError, onretry, value = $bindable([]), label, help, error, disabled = false }: Props = $props()

  const auto = $props.id()
  let query = $state('')
  // Expanded categories: those with a selection when the picker appears.
  let expanded = $state<ServiceCategory[]>([])
  let seeded = false
  $effect.pre(() => {
    if (seeded || !catalog) return
    seeded = true
    untrack(() => {
      expanded = CATEGORIES.filter((c) => catalog?.some((s) => s.category === c && value.includes(s.id)))
    })
  })

  const needle = $derived(query.trim().toLowerCase())
  const groups = $derived(
    CATEGORIES.map((c) => {
      const all = (catalog ?? []).filter((s) => s.category === c)
      return {
        category: c,
        total: all.length,
        selected: all.filter((s) => value.includes(s.id)).length,
        services: needle ? all.filter((s) => s.name.toLowerCase().includes(needle)) : all,
      }
    }).filter((g) => g.services.length > 0),
  )

  function setIds(next: string[]) {
    // Catalogue order; ids the catalogue does not know (any more) are kept at the end.
    const order = (catalog ?? []).map((s) => s.id)
    value = [...order.filter((x) => next.includes(x)), ...next.filter((x) => !order.includes(x))]
  }

  function toggle(id: string, on: boolean) {
    setIds(on ? [...value, id] : value.filter((x) => x !== id))
  }

  /** Selects (or clears) the shown services of a category. */
  function setAll(services: readonly ParentalService[], on: boolean) {
    const ids = services.map((s) => s.id)
    setIds(on ? [...value, ...ids.filter((id) => !value.includes(id))] : value.filter((x) => !ids.includes(x)))
  }

  function isOpen(c: ServiceCategory): boolean {
    return !!needle || expanded.includes(c)
  }

  function flip(c: ServiceCategory) {
    expanded = expanded.includes(c) ? expanded.filter((x) => x !== c) : [...expanded, c]
  }
</script>

<fieldset
  class="picker"
  aria-describedby={[error && `sp-${auto}-err`, help && `sp-${auto}-help`].filter(Boolean).join(' ') || undefined}
>
  <legend class="visually-hidden">{label}</legend>
  {#if catalogError && !catalog}
    <p class="error load" role="alert">
      <Icon name="error" size={16} />
      <span>{t('dns.parental.services.loadError', { error: errorText(catalogError) })}</span>
      {#if onretry}<Button size="sm" icon="refresh" onclick={onretry}>{t('common.action.retry')}</Button>{/if}
    </p>
  {:else if !catalog}
    <Skeleton height="120px" />
  {:else}
    <div class="top">
      <div class="search">
        <Input
          type="search"
          size="sm"
          icon="search"
          bind:value={query}
          placeholder={t('dns.parental.services.search')}
          aria-label={t('dns.parental.services.search')}
          autocomplete="off"
        />
      </div>
      <span class="count small muted" aria-live="polite">{tn('dns.parental.services.selected', value.length)}</span>
    </div>
    <div class="list">
      {#each groups as g (g.category)}
        {@const name = t(`dns.parental.category.${g.category}`)}
        {@const all = g.services.every((s) => value.includes(s.id))}
        {@const open = isOpen(g.category)}
        <section class="cat" aria-label={name}>
          <div class="head">
            <button
              type="button"
              class="disclose"
              aria-expanded={open}
              aria-controls="sp-{auto}-{g.category}"
              disabled={!!needle}
              onclick={() => flip(g.category)}
            >
              <span class={['chev', open && 'open']} aria-hidden="true"></span>
              <Icon name={CATEGORY_ICONS[g.category]} size={18} />
              <span class="cname">{name}</span>
              <span class="n small muted">{t('dns.parental.services.ofTotal', { selected: g.selected, total: g.total })}</span>
            </button>
            <button
              type="button"
              class="link small"
              {disabled}
              aria-label={all ? t('dns.parental.services.clearIn', { category: name }) : t('dns.parental.services.allIn', { category: name })}
              onclick={() => setAll(g.services, !all)}
            >
              {all ? t('dns.parental.services.clear') : t('dns.parental.services.all')}
            </button>
          </div>
          {#if open}
            <div class="items" id="sp-{auto}-{g.category}">
              {#each g.services as s (s.id)}
                <Checkbox checked={value.includes(s.id)} label={s.name} {disabled} onchange={(on) => toggle(s.id, on)} />
              {/each}
            </div>
          {/if}
        </section>
      {:else}
        <p class="small muted none">{t('dns.parental.services.noMatch', { query: query.trim() })}</p>
      {/each}
    </div>
  {/if}
  {#if error}
    <p class="error" id="sp-{auto}-err"><Icon name="alert" size={16} /><span>{error}</span></p>
  {/if}
  {#if help}<p class="help small muted" id="sp-{auto}-help">{help}</p>{/if}
</fieldset>

<style>
  .picker {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .top {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-3);
  }
  .search {
    flex: 1 1 200px;
    max-width: 320px;
  }
  .list {
    display: flex;
    flex-direction: column;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .none {
    padding: var(--sp-3);
  }
  .cat {
    min-width: 0;
  }
  .cat + .cat {
    border-top: 1px solid var(--line);
  }
  .head {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding-right: var(--sp-3);
  }
  .disclose {
    display: flex;
    flex: 1;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
    min-height: 40px;
    padding: var(--sp-2) var(--sp-3);
    border: 0;
    border-radius: var(--r-control);
    background: none;
    color: var(--text);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .disclose:disabled {
    cursor: default;
  }
  .disclose:hover:not(:disabled) {
    background: var(--surface-2);
  }
  .disclose:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: -2px;
  }
  .disclose :global(.icon) {
    color: var(--text-2);
  }
  .chev {
    flex: none;
    width: 6px;
    height: 6px;
    margin: 0 4px 0 2px;
    border-right: 1.5px solid var(--text-2);
    border-bottom: 1.5px solid var(--text-2);
    transform: rotate(-45deg);
    transition: transform var(--dur-fast);
  }
  .chev.open {
    transform: rotate(45deg);
  }
  .cname {
    min-width: 0;
    font-size: var(--fs-sm);
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .n {
    margin-left: auto;
    white-space: nowrap;
  }
  .link {
    flex: none;
    padding: 2px 0;
    border: 0;
    background: none;
    color: var(--focus);
    font: inherit;
    font-size: var(--fs-sm);
    text-decoration: underline;
    white-space: nowrap;
    cursor: pointer;
  }
  .link:disabled {
    color: var(--text-3);
    cursor: not-allowed;
  }
  .link:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .items {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(128px, 1fr));
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-1) var(--sp-3) var(--sp-3) calc(var(--sp-3) + 38px);
  }
  .error {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: 6px;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .load span {
    color: var(--text);
  }
  .error :global(.icon) {
    margin-top: 1px;
  }
  @media (max-width: 480px) {
    .items {
      padding-left: var(--sp-3);
    }
  }
</style>
