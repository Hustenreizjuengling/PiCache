<!--
  @component
  "Add blocklist": pick well-known lists from the built-in catalogue or
  enter any list URL with all settings. The catalogue is grouped by
  category with a search; each entry shows its description in the UI
  language, maintainer, homepage, size and memory estimate and the badges
  Recommended, Large and Added. Blocklists are added with one click (Default
  group); an allowlist asks for its groups first, because its entries win
  over every blocklist of those groups.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { i18n, t } from '$i18n/index.svelte'
  import {
    api,
    CATALOG_CATEGORIES,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type CatalogEntry,
    type ClientGroup,
    type FilterList,
    type FilterListInput,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Dialog, Icon, Input, Notice, Skeleton, Tabs, toast } from '$lib/ui'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import ListFields from './ListFields.svelte'
  import { categoryLabel, entriesText, isProtection, LARGE_ENTRIES } from './listStatus'

  interface Props {
    open?: boolean
    groups: readonly ClientGroup[] | undefined
    /** Lists that exist already (to mark catalogue entries as added). */
    lists: readonly FilterList[] | undefined
    onadded?: (list: FilterList) => void
  }

  let { open = $bindable(false), groups, lists, onadded }: Props = $props()

  const auto = $props.id()
  const formId = `add-list-${auto}`

  let tab = $state('catalog')
  let catalog = $state.raw<CatalogEntry[] | undefined>(undefined)
  let catalogErr = $state.raw<ApiError | undefined>(undefined)
  let query = $state('')
  let adding = $state('')
  /** The allowlist whose groups are being chosen, and its groups. */
  let picking = $state('')
  let pickGroups = $state<number[]>([])
  let draft = $state<FilterListInput>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  function blank(): FilterListInput {
    return {
      name: '',
      url: '',
      kind: 'block',
      plainDomains: 'exact',
      enabled: true,
      groupIds: [DEFAULT_GROUP_ID],
      comment: '',
      category: '',
      format: 'domains',
    }
  }

  // Every opening starts fresh; the catalogue is loaded once.
  $effect(() => {
    if (!open) return
    return untrack(() => {
      tab = 'catalog'
      query = ''
      picking = ''
      draft = blank()
      err = undefined
      submitted = false
      if (catalog) return
      const ctrl = new AbortController()
      api.filter
        .catalog({ signal: ctrl.signal })
        .then((c) => {
          catalog = c
          catalogErr = undefined
        })
        .catch((e) => {
          const ae = toApiError(e)
          if (ae.code !== 'aborted') catalogErr = ae
        })
      return () => ctrl.abort()
    })
  })

  const existing = $derived(new Set((lists ?? []).map((l) => l.url)))
  const existingKeys = $derived(new Set((lists ?? []).map((l) => l.catalogKey).filter(Boolean)))
  const needle = $derived(query.trim().toLowerCase())

  function description(e: CatalogEntry): string {
    return i18n.locale === 'de' && e.descriptionDe ? e.descriptionDe : e.description
  }

  function matches(e: CatalogEntry): boolean {
    if (!needle) return true
    return [e.name, e.description, e.descriptionDe, e.key].some((s) => s.toLowerCase().includes(needle))
  }

  /** Categories in API order; within one, recommended entries first, else catalogue order. */
  const sections = $derived(
    CATALOG_CATEGORIES.map((c) => ({
      category: c,
      entries: (catalog ?? [])
        .filter((e) => e.category === c && matches(e))
        .sort((a, b) => Number(b.recommended) - Number(a.recommended)),
    })).filter((s) => s.entries.length > 0),
  )

  function isAdded(e: CatalogEntry): boolean {
    return existing.has(e.url) || existingKeys.has(e.key)
  }

  async function create(e: CatalogEntry, groupIds: number[]) {
    adding = e.key
    try {
      const l = await api.filter.lists.create({
        name: e.name,
        url: e.url,
        kind: e.kind,
        category: e.category,
        plainDomains: e.plainDomains,
        enabled: true,
        groupIds,
        comment: '',
      })
      toast.success(t(e.kind === 'allow' ? 'dns.lists.addedAllow' : 'dns.lists.added', { name: l.name }))
      picking = ''
      onadded?.(l)
    } catch (err) {
      toast.error(err)
    } finally {
      adding = ''
    }
  }

  function add(e: CatalogEntry) {
    if (e.kind === 'allow') {
      picking = e.key
      pickGroups = []
      return
    }
    void create(e, [DEFAULT_GROUP_ID])
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = undefined
    if (!draft.url.trim() || saving || urlBlocked) return
    saving = true
    try {
      const l = await api.filter.lists.create({ ...draft, name: draft.name.trim(), url: draft.url.trim() })
      toast.success(t(l.kind === 'allow' ? 'dns.lists.addedAllow' : 'dns.lists.added', { name: l.name }))
      onadded?.(l)
      open = false
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }

  // An allowlist needs a group chosen on purpose.
  const urlBlocked = $derived(draft.kind === 'allow' && draft.groupIds.length === 0)
</script>

<Dialog bind:open title={t('dns.lists.addTitle')} size="lg" dismissible={!saving}>
  <Tabs
    bind:active={tab}
    label={t('dns.lists.addTitle')}
    tabs={[
      { id: 'catalog', label: t('dns.lists.fromCatalog') },
      { id: 'url', label: t('dns.lists.byUrl') },
    ]}
  >
    {#snippet children(active)}
      {#if active === 'catalog'}
        <div class="stack catalog">
          <p class="small muted">{t('dns.lists.catalogIntro')}</p>
          {#if catalogErr}
            <Notice tone="fail">{errorText(catalogErr)}</Notice>
          {:else if !catalog}
            <Skeleton height="200px" />
          {:else}
            <div class="search">
              <Input
                type="search"
                size="sm"
                icon="search"
                bind:value={query}
                placeholder={t('dns.lists.catalogSearch')}
                aria-label={t('dns.lists.catalogSearch')}
                autocomplete="off"
              />
            </div>
            {#each sections as sec (sec.category)}
              <section class="stack-sm" aria-labelledby="cat-{auto}-{sec.category}">
                <h3 id="cat-{auto}-{sec.category}">{categoryLabel(sec.category)}</h3>
                {#if sec.category === 'allow'}<Notice tone="info">{t('dns.lists.allowNotice')}</Notice>{/if}
                <ul>
                  {#each sec.entries as entry (entry.key)}
                    {@const added = isAdded(entry)}
                    <li>
                      <div class="line">
                        <div class="entry">
                          <p class="name" title={entry.url}>
                            <span>{entry.name}</span>
                            {#if entry.recommended}<Badge tone="ok">{t('dns.lists.recommended')}</Badge>{/if}
                            {#if entry.entries > LARGE_ENTRIES}
                              <Badge tone="warn" title={t('dns.lists.largeHelp')}>{t('dns.lists.large')}</Badge>
                            {/if}
                            {#if added}<Badge>{t('dns.lists.alreadyAdded')}</Badge>{/if}
                          </p>
                          <p class="small muted">{description(entry)}</p>
                          <p class="xsmall subtle meta">
                            <span>{entry.maintainer}</span>
                            {#if entry.homepage}
                              <a href={entry.homepage} target="_blank" rel="noopener noreferrer">
                                {t('dns.lists.homepage')}<Icon name="external" size={12} />
                              </a>
                            {/if}
                            <span>{entriesText(entry.entries)}</span>
                            {#if entry.license}<span>{entry.license}</span>{/if}
                          </p>
                          {#if isProtection(entry.category)}
                            <p class="xsmall protect"><Icon name="shield" size={14} /><span>{t('dns.lists.protectionNotice')}</span></p>
                          {/if}
                        </div>
                        {#if !added && picking !== entry.key}
                          <Button
                            size="sm"
                            icon="plus"
                            loading={adding === entry.key}
                            disabled={!session.isAdmin || (adding !== '' && adding !== entry.key)}
                            onclick={() => add(entry)}
                          >
                            {t('common.action.add')}
                          </Button>
                        {/if}
                      </div>
                      {#if picking === entry.key && !added}
                        <div class="pick stack-sm">
                          <GroupPicker
                            {groups}
                            bind:value={pickGroups}
                            label={t('dns.lists.allowGroups')}
                            emptyWarning={t('dns.lists.allowGroupsRequired')}
                          />
                          <div class="row pick-actions">
                            <Button size="sm" variant="ghost" disabled={adding === entry.key} onclick={() => (picking = '')}>
                              {t('common.action.cancel')}
                            </Button>
                            <Button
                              size="sm"
                              variant="primary"
                              icon="plus"
                              loading={adding === entry.key}
                              disabled={pickGroups.length === 0 || !session.isAdmin}
                              onclick={() => create(entry, pickGroups)}
                            >
                              {t('dns.lists.addAllow')}
                            </Button>
                          </div>
                        </div>
                      {/if}
                    </li>
                  {/each}
                </ul>
              </section>
            {:else}
              <p class="small muted">{t('dns.lists.catalogNoMatch', { query: query.trim() })}</p>
            {/each}
          {/if}
        </div>
      {:else}
        <form id={formId} class="stack url-form" onsubmit={submit} novalidate>
          {#if err && !err.field}<Notice tone="fail">{errorText(err)}</Notice>{/if}
          <fieldset disabled={!session.isAdmin} class="stack">
            <ListFields bind:draft {err} {groups} {submitted} fresh />
          </fieldset>
        </form>
      {/if}
    {/snippet}
  </Tabs>

  {#snippet actions()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>
      {tab === 'url' ? t('common.action.cancel') : t('common.action.close')}
    </Button>
    {#if tab === 'url'}
      <Button type="submit" form={formId} variant="primary" icon="plus" loading={saving} disabled={!session.isAdmin || urlBlocked}>
        {draft.kind === 'allow' ? t('dns.lists.addAllow') : t('dns.lists.add')}
      </Button>
    {/if}
  {/snippet}
</Dialog>

<style>
  .catalog,
  .url-form {
    padding-top: var(--sp-4);
  }
  .search {
    max-width: 360px;
  }
  h3 {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  ul {
    margin: 0;
    padding: 0;
    list-style: none;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  li {
    padding: var(--sp-3);
  }
  li + li {
    border-top: 1px solid var(--line);
  }
  .line {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
  }
  .entry {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .name {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-3);
  }
  .meta a {
    display: inline-flex;
    align-items: center;
    gap: 2px;
  }
  .protect {
    display: flex;
    align-items: flex-start;
    gap: 4px;
    color: var(--text-2);
  }
  .protect :global(.icon) {
    margin-top: 1px;
  }
  .pick {
    margin-top: var(--sp-3);
    padding: var(--sp-3);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .pick-actions {
    justify-content: flex-end;
  }
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  @media (max-width: 480px) {
    .line {
      flex-direction: column;
      align-items: flex-start;
    }
  }
</style>
