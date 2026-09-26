<!--
  @component
  The category switches of a group (adult content, gambling, dating,
  piracy, bypass): each assigns one downloaded catalogue list to the group.
  Shows the list's name and size and, for a saved switch, a warning while
  the list waits for its first download or has failed without a copy. A
  bound list that was given another category (it then pauses with
  blocking, so the switch is off) is named with what switching on does.
  Bound to the switch states of the draft.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import type { CatalogEntry, CategorySwitch, FilterList, GroupControls } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import { Icon, Toggle } from '$lib/ui'
  import { categoryLabel, entriesText } from '../filtering/listStatus'
  import { switchesOf } from './plan'

  interface Props {
    value: Partial<Record<CategorySwitch, boolean>>
    /** The saved state (for the download warnings). */
    group: GroupControls
    filterCatalog: readonly CatalogEntry[] | undefined
    lists: readonly FilterList[] | undefined
  }

  let { value = $bindable(), group, filterCatalog, lists }: Props = $props()

  const rows = $derived(
    switchesOf(group).map((s) => {
      const entry = filterCatalog?.find((e) => e.key === s.catalogKey)
      const list = lists?.find((l) => l.catalogKey === s.catalogKey || (!!entry && l.url === entry.url))
      const entries = list && list.entries > 0 ? list.entries : entry?.entries
      return { ...s, name: list?.name ?? entry?.name, entries, list, saved: group.categories[s.id] }
    }),
  )
</script>

<ul class="switches">
  {#each rows as r (r.id)}
    {@const kept = !!value[r.id] && !!r.saved?.on}
    <li>
      <Toggle
        bind:checked={() => !!value[r.id], (on) => (value[r.id] = on)}
        label={t(`dns.parental.switch.${r.id}` as MessageKey)}
        description={t(`dns.parental.switch.${r.id}Help` as MessageKey)}
      />
      {#if r.name}
        <p class="meta xsmall muted">{r.entries ? `${r.name} · ${entriesText(r.entries)}` : r.name}</p>
      {/if}
      {#if r.list && r.list.kind === 'block' && r.list.category !== r.category && !r.saved?.on}
        <p class="state small muted">
          <Icon name="info" size={16} />
          <span>{t('dns.parental.switch.otherCategory', { name: r.list.name, category: categoryLabel(r.list.category) })}</span>
        </p>
      {/if}
      {#if kept && r.saved?.state === 'pending'}
        <p class="state warn small"><Icon name="clock" size={16} /><span>{t('dns.parental.switch.pending')}</span></p>
      {:else if kept && r.saved?.state === 'failed'}
        <p class="state fail small">
          <Icon name="alert" size={16} />
          <span>
            {t('dns.parental.switch.failed')}
            {#if r.list}<a href={href('/dns/filtering', { tab: 'lists', sel: r.list.id })}>{t('dns.parental.switch.openList')}</a>{/if}
          </span>
        </p>
      {/if}
    </li>
  {/each}
</ul>

<style>
  .switches {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  /* Aligned with the toggle's text (track 36 px + gap). */
  .meta,
  .state {
    padding-left: calc(36px + var(--sp-3));
    overflow-wrap: anywhere;
  }
  .state {
    display: flex;
    align-items: flex-start;
    gap: 6px;
  }
  .state :global(.icon) {
    flex: none;
    margin-top: 1px;
  }
  .warn {
    color: var(--warning);
  }
  .fail {
    color: var(--danger);
  }
</style>
