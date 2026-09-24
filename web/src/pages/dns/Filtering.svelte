<!--
  @component
  Filtering: blocklists, your own rules and the "Why is this blocked?"
  tester, with the state of the compiled filter on top.
  Query: ?tab=lists|rules|test (+ the tab's own parameters)
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { formatBytes, formatNumber, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { Notice, Spinner, Tabs } from '$lib/ui'
  import ListsTab from './filtering/ListsTab.svelte'
  import RulesTab from './filtering/RulesTab.svelte'
  import TesterTab from './filtering/TesterTab.svelte'

  const TABS = ['lists', 'rules', 'test'] as const
  type Tab = (typeof TABS)[number]

  const tab = $derived.by((): Tab => {
    const v = router.param('tab') as Tab
    return TABS.includes(v) ? v : 'lists'
  })

  const groups = resource((signal) => api.groups.list({ signal }))
  const stats = resource((signal) => api.filter.stats({ signal }), { interval: 10_000 })

  const tabs = $derived([
    { id: 'lists', label: t('dns.filtering.tab.lists'), count: stats.data?.lists },
    { id: 'rules', label: t('dns.filtering.tab.rules'), count: stats.data?.rules },
    { id: 'test', label: t('dns.filtering.tab.test') },
  ])

  function selectTab(id: string) {
    // Each tab has its own query parameters; start clean.
    router.navigate('/dns/filtering', id === 'lists' ? {} : { tab: id }, { replace: true })
  }

  function changed() {
    void stats.refresh()
  }
</script>

<div class="page">
  {#if stats.data}
    {@const s = stats.data}
    <p class="summary small muted">
      {#if s.updating}<span class="updating"><Spinner size={14} />{t('dns.filtering.updating')}</span>{/if}
      <span>{tn('dns.filtering.summary.lists', s.lists, { count: formatNumber(s.lists) })}</span>
      <span aria-hidden="true">·</span>
      <span>{tn('dns.filtering.summary.entries', s.entries, { count: formatNumber(s.entries) })}</span>
      <span aria-hidden="true">·</span>
      <span>{tn('dns.filtering.summary.patterns', s.patterns, { count: formatNumber(s.patterns) })}</span>
      <span aria-hidden="true">·</span>
      <span>{tn('dns.filtering.summary.rules', s.rules, { count: formatNumber(s.rules) })}</span>
      {#if s.compiledAt}
        <span aria-hidden="true">·</span>
        <span>
          {t('dns.filtering.summary.compiled', {
            when: formatRelative(s.compiledAt),
            ms: formatNumber(s.compileMs),
            memory: formatBytes(s.memoryBytes),
          })}
        </span>
      {/if}
    </p>
    {#if s.failedLists > 0}
      <Notice tone="warn">{tn('dns.filtering.failedLists', s.failedLists)}</Notice>
    {/if}
    {#if s.staleLists > 0}
      <Notice tone="warn">{tn('dns.filtering.staleLists', s.staleLists)}</Notice>
    {/if}
    {#if s.patternsDropped > 0}
      <Notice tone="warn">{tn('dns.filtering.patternsDropped', s.patternsDropped)}</Notice>
    {/if}
  {/if}

  <Tabs {tabs} active={tab} label={t('common.nav.filtering')} onchange={selectTab}>
    {#snippet children(active)}
      <div class="tab">
        {#if active === 'rules'}
          <RulesTab groups={groups.data} onchanged={changed} />
        {:else if active === 'test'}
          <TesterTab groups={groups.data} />
        {:else}
          <ListsTab groups={groups.data} updating={!!stats.data?.updating} onchanged={changed} />
        {/if}
      </div>
    {/snippet}
  </Tabs>
</div>

<style>
  .summary {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  .updating {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
    color: var(--text);
  }
  .tab {
    padding-top: var(--sp-4);
  }
</style>
