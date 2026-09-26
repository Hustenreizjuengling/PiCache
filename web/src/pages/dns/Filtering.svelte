<!--
  @component
  Filtering: blocklists, your own rules (for domains and for the addresses
  of answers) and the "Why is this blocked?" tester with the list search,
  with the state of the compiled filter on top.
  Query: ?tab=lists|rules|test, &view=ip (rules: the IP rules), &mode=search
  (test: the list search) (+ the tab's own parameters)
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { formatBytes, formatNumber, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { Notice, Segmented, Spinner, Tabs } from '$lib/ui'
  import IpRulesTab from './filtering/IpRulesTab.svelte'
  import ListSearch from './filtering/ListSearch.svelte'
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
  // The tab badges count what the tabs list, disabled entries included; the
  // summary line counts what the compiled filter uses.
  const lists = resource((signal) => api.filter.lists.list({ signal }))
  const rules = resource((signal) => api.filter.rules.list({}, { signal }))
  const ipRules = resource((signal) => api.filter.ipRules.list({}, { signal }))

  const ruleView = $derived(router.param('view') === 'ip' ? 'ip' : 'domains')
  const ruleCount = $derived(rules.data && ipRules.data ? rules.data.length + ipRules.data.length : rules.data?.length)

  const tabs = $derived([
    { id: 'lists', label: t('dns.filtering.tab.lists'), count: lists.data?.length },
    { id: 'rules', label: t('dns.filtering.tab.rules'), count: ruleCount },
    { id: 'test', label: t('dns.filtering.tab.test') },
  ])

  function selectTab(id: string) {
    // Each tab has its own query parameters; start clean.
    router.navigate('/dns/filtering', id === 'lists' ? {} : { tab: id }, { replace: true })
  }

  // The views of the rules tab and the modes of the test tab have their own
  // parameters: start clean.
  function selectView(v: string) {
    router.navigate('/dns/filtering', v === 'ip' ? { tab: 'rules', view: 'ip' } : { tab: 'rules' }, { replace: true })
  }

  const testMode = $derived(router.param('mode') === 'search' ? 'search' : 'test')

  function selectTestMode(m: string) {
    router.navigate('/dns/filtering', m === 'search' ? { tab: 'test', mode: 'search' } : { tab: 'test' }, { replace: true })
  }

  function changed() {
    void stats.refresh()
    void lists.refresh()
    void rules.refresh()
    void ipRules.refresh()
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
      <span title={t('dns.filtering.summary.patternsHint')}>{tn('dns.filtering.summary.patterns', s.patterns, { count: formatNumber(s.patterns) })}</span>
      <span aria-hidden="true">·</span>
      <span>{tn('dns.filtering.summary.rules', s.rules, { count: formatNumber(s.rules) })}</span>
      {#if s.ipEntries > 0}
        <span aria-hidden="true">·</span>
        <span title={t('dns.filtering.summary.ipEntriesHint')}>{tn('dns.filtering.summary.ipEntries', s.ipEntries, { count: formatNumber(s.ipEntries) })}</span>
      {/if}
      {#if s.ipRules > 0}
        <span aria-hidden="true">·</span>
        <span>{tn('dns.filtering.summary.ipRules', s.ipRules, { count: formatNumber(s.ipRules) })}</span>
      {/if}
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
    {#if s.modifiedDropped > 0}
      <Notice tone="warn">{tn('dns.filtering.modifiedDropped', s.modifiedDropped, { count: formatNumber(s.modifiedDropped) })}</Notice>
    {/if}
  {/if}

  <Tabs {tabs} active={tab} label={t('common.nav.filtering')} onchange={selectTab}>
    {#snippet children(active)}
      <div class="tab">
        {#if active === 'rules'}
          <div class="stack">
            <Segmented
              label={t('dns.rules.view')}
              value={ruleView}
              options={[
                { value: 'domains', label: t('dns.rules.view.domains') },
                { value: 'ip', label: t('dns.rules.view.ip') },
              ]}
              onchange={selectView}
            />
            {#if ruleView === 'ip'}
              <IpRulesTab groups={groups.data} onchanged={changed} />
            {:else}
              <RulesTab groups={groups.data} onchanged={changed} />
            {/if}
          </div>
        {:else if active === 'test'}
          <div class="stack">
            <Segmented
              label={t('dns.tester.mode')}
              value={testMode}
              options={[
                { value: 'test', label: t('dns.tester.mode.test') },
                { value: 'search', label: t('dns.tester.mode.search') },
              ]}
              onchange={selectTestMode}
            />
            {#if testMode === 'search'}
              <ListSearch groups={groups.data} />
            {:else}
              <TesterTab groups={groups.data} />
            {/if}
          </div>
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
