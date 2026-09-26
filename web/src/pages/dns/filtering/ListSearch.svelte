<!--
  @component
  "Search lists": finds your rules, IP rules and the lines of the
  downloaded lists that contain a text (at least 3 characters), optionally
  marking what applies to a client. The server stops after 200 hits or 10
  seconds and then says how many lists it searched; while two searches or
  explanations run it asks to try again.
  Query: ?tab=test&mode=search&q=…&client=<ip>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type ClientGroup, type SearchItem } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatNumber } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { Badge, Button, Chip, EmptyState, Field, Input, Notice, Panel, Table, type Column } from '$lib/ui'
  import { groupNames } from '../shared/groups'
  import { typesText } from './rules'

  let { groups }: { groups: readonly ClientGroup[] | undefined } = $props()

  const MIN = 3

  const reqQ = $derived(router.param('q').trim().toLowerCase())
  const reqClient = $derived(router.param('client').trim())

  const result = resource((signal) =>
    reqQ.length >= MIN ? api.filter.search({ q: reqQ, clientIp: reqClient || undefined }, { signal }) : Promise.resolve(undefined),
  )

  let q = $state(untrack(() => reqQ))
  let client = $state(untrack(() => reqClient))
  let submitted = $state(false)

  $effect(() => {
    const [a, b] = [reqQ, reqClient]
    untrack(() => {
      q = a
      client = b
    })
  })

  const qError = $derived(
    fieldError(result.error, 'q') ??
      (submitted && q.trim().length < MIN ? t('dns.search.tooShort', { count: MIN }) : undefined),
  )
  const clientError = $derived(fieldError(result.error, 'clientIp'))
  const busy = $derived(result.error?.code === 'unavailable')
  const otherError = $derived(result.error && !result.error.field && !busy ? errorText(result.error) : undefined)

  function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    const text = q.trim().toLowerCase()
    if (text.length < MIN) return
    q = text
    if (text === reqQ && client.trim() === reqClient) void result.refresh()
    else router.setQuery({ q: text, client: client.trim() })
  }

  const res = $derived(reqQ ? result.data : undefined)
  const withClient = $derived(!!res && !!reqClient)

  type Row = SearchItem & { i: number }
  const rows = $derived<Row[]>((res?.items ?? []).map((x, i) => ({ ...x, i })))

  function sourceLabel(x: SearchItem): string {
    return x.source === 'rule' ? t('dns.search.source.rule') : x.source === 'ip-rule' ? t('dns.search.source.ipRule') : t('dns.search.source.list')
  }

  function sourceHref(x: SearchItem): string {
    if (x.source === 'rule') return href('/dns/filtering', { tab: 'rules', sel: x.ruleId })
    if (x.source === 'ip-rule') return href('/dns/filtering', { tab: 'rules', view: 'ip', sel: x.ruleId })
    return href('/dns/filtering', { tab: 'lists', sel: x.listId })
  }

  const columns = $derived<Column<Row>[]>([
    { key: 'source', label: t('dns.shared.match.source'), width: '1%', value: sourceLabel, cell: sourceCell },
    { key: 'name', label: t('common.label.name'), truncate: true, width: '30%', value: (x) => x.name, cell: nameCell },
    { key: 'entry', label: t('dns.shared.match.pattern'), mono: true, truncate: true, width: '50%', value: (x) => x.entry, cell: entryCell },
    { key: 'action', label: t('dns.rules.action'), width: '1%', value: (x) => x.action, cell: actionCell },
    ...(withClient ? [{ key: 'applies', label: t('dns.search.applies'), width: '1%', value: (x: Row) => x.applies, cell: appliesCell }] : []),
  ])
</script>

{#snippet sourceCell(x: Row)}
  <span class="nowrap">{sourceLabel(x)}</span>
{/snippet}

{#snippet nameCell(x: Row)}
  <a href={sourceHref(x)} title={x.name}>{x.name}</a>
{/snippet}

{#snippet entryCell(x: Row)}
  <span class="entry" title={x.entry}>
    <span class="text">{x.entry}</span>
    {#if x.qtypes.length > 0}<Badge>{typesText(x.qtypes, x.qtypesNegate)}</Badge>{/if}
  </span>
{/snippet}

{#snippet actionCell(x: Row)}
  <Chip
    size="sm"
    pair={x.action === 'block' ? 'orange' : 'blue'}
    label={x.action === 'block' ? t('dns.rules.action.block') : t('dns.rules.action.allow')}
  />
{/snippet}

{#snippet appliesCell(x: Row)}
  <span class={x.applies ? 'nowrap' : 'nowrap subtle'} title={groupNames(x.groupIds, groups)}>
    {x.applies ? t('dns.search.appliesYes') : x.enabled ? t('dns.search.appliesNo') : t('dns.search.appliesDisabled')}
  </span>
{/snippet}

<div class="stack">
  <Panel title={t('dns.search.title')} description={t('dns.search.description')}>
    <form class="form" onsubmit={submit} novalidate>
      <div class="q">
        <Field label={t('dns.search.query')} error={qError} required>
          <Input bind:value={q} mono type="search" placeholder="doubleclick" maxlength={253} autocomplete="off" />
        </Field>
      </div>
      <div class="client">
        <Field label={t('dns.tester.client')} optional error={clientError} help={t('dns.search.clientHelp')}>
          <Input bind:value={client} mono placeholder={t('dns.tester.clientPlaceholder')} maxlength={64} autocomplete="off" />
        </Field>
      </div>
      <div class="go">
        <Button type="submit" variant="primary" icon="search" loading={result.loading}>{t('dns.search.run')}</Button>
      </div>
    </form>
  </Panel>

  {#if busy}
    <Notice tone="warn" title={t('dns.search.busyTitle')}>
      {errorText(result.error)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" loading={result.loading} onclick={() => result.refresh()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {/if}
  {#if otherError}<Notice tone="fail">{otherError}</Notice>{/if}

  {#if res}
    <Panel flush title={t('dns.search.resultTitle', { q: res.q })}>
      <div class="stack-sm meta">
        <p class="small muted">
          {t('dns.search.summary', {
            hits: formatNumber(res.items.length),
            scanned: formatNumber(res.scannedLists),
            total: formatNumber(res.totalLists),
          })}
        </p>
        {#if res.truncated || res.timedOut}
          <Notice tone="warn" title={t('dns.search.incomplete')}>
            {res.timedOut
              ? t('dns.search.timedOut', { scanned: formatNumber(res.scannedLists), total: formatNumber(res.totalLists) })
              : t('dns.search.truncated', { count: formatNumber(res.items.length) })}
          </Notice>
        {/if}
      </div>
      <Table {columns} {rows} key={(x) => x.i} caption={t('dns.search.resultTitle', { q: res.q })}>
        {#snippet empty()}
          <EmptyState compact title={t('dns.search.empty')} text={t('dns.search.emptyText')} />
        {/snippet}
      </Table>
    </Panel>
  {:else if !reqQ && !result.loading}
    <Panel>
      <EmptyState icon="search" title={t('dns.search.emptyTitle')} text={t('dns.search.emptyIntro')} />
    </Panel>
  {/if}
</div>

<style>
  .form {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .q {
    flex: 2 1 260px;
  }
  .client {
    flex: 1 1 200px;
  }
  .go {
    padding-top: 26px;
  }
  .meta {
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .entry {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    max-width: 100%;
  }
  .text {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  @media (max-width: 480px) {
    .q,
    .client {
      flex: 1 1 100%;
    }
    .go {
      padding-top: 0;
    }
  }
</style>
