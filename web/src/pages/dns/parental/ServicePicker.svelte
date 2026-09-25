<!--
  @component
  Chooses services from the built-in catalogue: a search box (service names)
  and one checklist per category. Bound to the selected ids in catalogue
  order. Used for the always-blocked services and for schedules that block
  selected services.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { ApiError, ParentalService } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { Button, Checkbox, Icon, Input, Skeleton } from '$lib/ui'
  import { CATEGORIES } from './plan'

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

  const needle = $derived(query.trim().toLowerCase())
  const groups = $derived(
    CATEGORIES.map((c) => ({
      category: c,
      services: (catalog ?? []).filter((s) => s.category === c && (!needle || s.name.toLowerCase().includes(needle))),
    })).filter((g) => g.services.length > 0),
  )

  function toggle(id: string, on: boolean) {
    const next = on ? [...value, id] : value.filter((x) => x !== id)
    // Catalogue order; ids the catalogue does not know (any more) are kept at the end.
    const order = (catalog ?? []).map((s) => s.id)
    value = [...order.filter((x) => next.includes(x)), ...next.filter((x) => !order.includes(x))]
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
        <fieldset class="cat">
          <legend>{t(`dns.parental.category.${g.category}`)}</legend>
          <div class="items">
            {#each g.services as s (s.id)}
              <Checkbox checked={value.includes(s.id)} label={s.name} {disabled} onchange={(on) => toggle(s.id, on)} />
            {/each}
          </div>
        </fieldset>
      {:else}
        <p class="small muted">{t('dns.parental.services.noMatch', { query: query.trim() })}</p>
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
    gap: var(--sp-3);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .cat {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .cat legend {
    padding: 0;
    margin-bottom: var(--sp-2);
    color: var(--text-2);
    font-size: var(--fs-xs);
    font-weight: 600;
  }
  .items {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(128px, 1fr));
    gap: var(--sp-2) var(--sp-3);
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
</style>
