<!--
  @component
  "Why is this blocked?": the decision in one sentence and every list or
  rule entry matching the domain (from /filter/explain or /dns/lookup), in
  precedence order, with the decisive one marked, their query types,
  exceptions, inversion and answer, and why an entry that matches the name
  does not apply (another query type, an exception).
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { ClientGroup, FilterMatch } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import { Badge, Chip, Table, type Column } from '$lib/ui'
  import { replyShort, typesText } from '../filtering/rules'
  import { groupNames } from './groups'

  interface Props {
    matches: readonly FilterMatch[]
    groups: readonly ClientGroup[] | undefined
    /** Group ids the domain was evaluated for (shown in the summary). */
    groupIds?: readonly number[]
    compact?: boolean
  }

  let { matches, groups, groupIds, compact = true }: Props = $props()

  type Row = FilterMatch & { idx: number }

  const rows = $derived<Row[]>(matches.map((m, idx) => ({ ...m, idx })))
  const decisive = $derived(matches.find((m) => m.decisive))

  const summary = $derived.by(() => {
    const m = decisive
    if (!m) return t('dns.shared.match.none')
    const block = m.action === 'block'
    if (m.source === 'rule') return t(block ? 'dns.shared.match.blockedRule' : 'dns.shared.match.allowedRule', { pattern: m.pattern })
    return t(block ? 'dns.shared.match.blockedList' : 'dns.shared.match.allowedList', { name: m.name })
  })

  function sourceHref(m: FilterMatch): string {
    return m.source === 'rule'
      ? href('/dns/filtering', { tab: 'rules', sel: m.ruleId })
      : href('/dns/filtering', { tab: 'lists', sel: m.listId })
  }

  function kindLabel(kind: string): string {
    switch (kind) {
      case 'exact':
        return t('dns.shared.kind.exact')
      case 'subtree':
        return t('dns.shared.kind.subtree')
      case 'regex':
        return t('dns.shared.kind.regex')
    }
    return kind
  }

  function hasOptions(m: FilterMatch): boolean {
    return m.qtypes.length > 0 || m.denyallow.length > 0 || m.invert || !!m.reply
  }

  // The options column only when an entry has options.
  const withOptions = $derived(matches.some(hasOptions))

  const columns: Column<Row>[] = $derived([
    { key: 'effect', label: t('dns.shared.match.effect'), cell: effectCell },
    { key: 'source', label: t('dns.shared.match.source'), cell: sourceCell },
    { key: 'kind', label: t('dns.shared.match.kind'), value: (m) => kindLabel(m.kind) },
    { key: 'pattern', label: t('dns.shared.match.pattern'), mono: true, truncate: true, width: '34%', value: (m) => m.pattern },
    ...(withOptions ? [{ key: 'options', label: t('dns.rules.options'), cell: optionsCell }] : []),
    { key: 'groups', label: t('common.label.groups'), value: (m: Row) => groupNames(m.groupIds, groups) },
  ])
</script>

{#snippet optionsCell(m: Row)}
  {@const reply = replyShort(m)}
  <span class="effect">
    {#if m.qtypes.length > 0}<Badge title={t('dns.rules.qtypes')}>{typesText(m.qtypes, m.qtypesNegate)}</Badge>{/if}
    {#if reply}<Badge tone="info" title={t('dns.rules.reply')}>{reply}</Badge>{/if}
    {#if m.denyallow.length > 0}
      <Badge title={t('dns.rules.badge.exceptHelp', { domains: m.denyallow.join(', ') })}>{tn('dns.rules.badge.except', m.denyallow.length)}</Badge>
    {/if}
    {#if m.invert}<Badge tone="warn" title={t('dns.rules.invertHelp')}>{t('dns.rules.badge.invert')}</Badge>{/if}
    {#if !hasOptions(m)}<span class="subtle">–</span>{/if}
  </span>
{/snippet}

{#snippet effectCell(m: Row)}
  <span class="effect">
    {#if m.decisive}
      <Chip
        size="sm"
        pair={m.action === 'block' ? 'orange' : 'blue'}
        label={m.action === 'block' ? t('dns.shared.match.blocks') : t('dns.shared.match.allows')}
      />
    {:else if m.skipped === 'qtype'}
      <span class="subtle" title={t('dns.shared.match.skippedQtypeHelp')}>{t('dns.shared.match.skippedQtype')}</span>
    {:else if m.skipped === 'denyallow'}
      <span class="subtle" title={t('dns.shared.match.skippedDenyallowHelp')}>{t('dns.shared.match.skippedDenyallow')}</span>
    {:else if m.applies}
      <span class="muted">{m.action === 'block' ? t('dns.shared.match.blockOverridden') : t('dns.shared.match.allowOverridden')}</span>
    {:else}
      <span class="subtle">{t('dns.shared.match.otherGroups')}</span>
    {/if}
    {#if m.important}<Badge title={t('dns.shared.match.importantHelp')}>{t('dns.shared.match.important')}</Badge>{/if}
  </span>
{/snippet}

{#snippet sourceCell(m: Row)}
  {#if m.source === 'rule'}
    <a href={sourceHref(m)}>{t('dns.shared.match.rule')}</a>
  {:else}
    <a href={sourceHref(m)}>{m.name}</a>
  {/if}
{/snippet}

<div class="matches">
  <p class="summary">{summary}</p>
  {#if groupIds}
    <p class="small muted">{t('dns.shared.match.groups', { groups: groupNames(groupIds, groups) })}</p>
  {/if}
  {#if matches.length > 0}
    <div class="table">
      <Table {columns} {rows} key={(m) => m.idx} {compact} caption={t('dns.shared.match.caption')} />
    </div>
  {/if}
</div>

<style>
  .matches {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
  }
  .summary {
    font-weight: 600;
  }
  .table {
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    overflow: hidden;
  }
  .effect {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1);
  }
</style>
