<!--
  @component
  "Add blocklist": pick well-known lists from the built-in catalogue (one
  click each) or enter any list URL with all settings.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, type MessageKey } from '$i18n/index.svelte'
  import {
    api,
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
  import { Badge, Button, Dialog, Notice, Skeleton, Tabs, toast } from '$lib/ui'
  import ListFields from './ListFields.svelte'

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
  const CATEGORIES = ['general', 'security', 'privacy', 'other'] as const

  let tab = $state('catalog')
  let catalog = $state.raw<CatalogEntry[] | undefined>(undefined)
  let catalogErr = $state.raw<ApiError | undefined>(undefined)
  let adding = $state('')
  let draft = $state<FilterListInput>(blank())
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  function blank(): FilterListInput {
    return { name: '', url: '', kind: 'block', plainDomains: 'exact', enabled: true, groupIds: [DEFAULT_GROUP_ID], comment: '' }
  }

  // Every opening starts fresh; the catalogue is loaded once.
  $effect(() => {
    if (!open) return
    return untrack(() => {
      tab = 'catalog'
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

  function categoryLabel(c: string): string {
    return t(`dns.lists.category.${c}` as MessageKey)
  }

  async function addEntry(e: CatalogEntry) {
    adding = e.key
    try {
      const l = await api.filter.lists.create({
        name: e.name,
        url: e.url,
        kind: 'block',
        plainDomains: e.plainDomains,
        enabled: true,
        groupIds: [DEFAULT_GROUP_ID],
        comment: '',
      })
      toast.success(t('dns.lists.added', { name: l.name }))
      onadded?.(l)
    } catch (err) {
      toast.error(err)
    } finally {
      adding = ''
    }
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = undefined
    if (!draft.url.trim() || saving) return
    saving = true
    try {
      const l = await api.filter.lists.create({ ...draft, name: draft.name.trim(), url: draft.url.trim() })
      toast.success(t('dns.lists.added', { name: l.name }))
      onadded?.(l)
      open = false
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }
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
            {#each CATEGORIES as cat (cat)}
              {@const entries = catalog.filter((c) => c.category === cat)}
              {#if entries.length > 0}
                <section class="stack-sm" aria-label={categoryLabel(cat)}>
                  <h3>{categoryLabel(cat)}</h3>
                  <ul>
                    {#each entries as entry (entry.key)}
                      <li>
                        <div class="entry">
                          <p class="name">
                            {entry.name}
                            {#if entry.recommended}<Badge tone="ok">{t('dns.lists.recommended')}</Badge>{/if}
                          </p>
                          <p class="small muted">{entry.description}</p>
                          <p class="xsmall subtle mono url">{entry.url}</p>
                        </div>
                        {#if existing.has(entry.url)}
                          <span class="small muted added">{t('dns.lists.alreadyAdded')}</span>
                        {:else}
                          <Button
                            size="sm"
                            icon="plus"
                            loading={adding === entry.key}
                            disabled={!session.isAdmin || (adding !== '' && adding !== entry.key)}
                            onclick={() => addEntry(entry)}
                          >
                            {t('common.action.add')}
                          </Button>
                        {/if}
                      </li>
                    {/each}
                  </ul>
                </section>
              {/if}
            {/each}
          {/if}
        </div>
      {:else}
        <form id={formId} class="stack url-form" onsubmit={submit} novalidate>
          {#if err && !err.field}<Notice tone="fail">{errorText(err)}</Notice>{/if}
          <fieldset disabled={!session.isAdmin} class="stack">
            <ListFields bind:draft {err} {groups} {submitted} />
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
      <Button type="submit" form={formId} variant="primary" icon="plus" loading={saving} disabled={!session.isAdmin}>
        {t('dns.lists.add')}
      </Button>
    {/if}
  {/snippet}
</Dialog>

<style>
  .catalog,
  .url-form {
    padding-top: var(--sp-4);
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
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    padding: var(--sp-3);
  }
  li + li {
    border-top: 1px solid var(--line);
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
  }
  .url {
    overflow-wrap: anywhere;
  }
  .added {
    flex: none;
  }
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
</style>
