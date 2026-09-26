<!--
  @component
  "Why is this blocked?": runs a test query through the whole pipeline as a
  given client (POST /dns/lookup, not logged) and shows the answer, every
  step of the trace and every list or rule entry that matches the domain,
  with the decisive one marked (with their query types, exceptions,
  inversion and answer, and the entries that match the name but not this
  query type or name).
  Query: ?domain=…&client=<ip>&qtype=A
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, resource, type ClientGroup, type FilterRuleInput } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatMicros } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { isBlockedStatus } from '$lib/traffic'
  import { Button, EmptyState, Field, Input, KeyValue, Notice, Panel, QueryStatusChip, Select } from '$lib/ui'
  import { groupNames } from '../shared/groups'
  import { asciiDomain } from '../shared/input'
  import MatchList from '../shared/MatchList.svelte'
  import RulePanel from './RulePanel.svelte'

  let { groups }: { groups: readonly ClientGroup[] | undefined } = $props()

  const TYPES = ['A', 'AAAA', 'HTTPS', 'SVCB', 'CNAME', 'MX', 'TXT', 'PTR', 'SRV', 'NS', 'ANY']

  // Separate values: a request only reruns when one of them really changes.
  const reqName = $derived(router.param('domain').trim())
  const reqType = $derived(router.param('qtype').trim().toUpperCase() || 'A')
  const reqClient = $derived(router.param('client').trim())

  const result = resource((signal) =>
    reqName
      ? api.dns.lookup({ name: reqName, type: reqType, clientIp: reqClient || undefined }, { signal })
      : Promise.resolve(undefined),
  )

  let domain = $state(untrack(() => reqName))
  let qtype = $state(untrack(() => reqType))
  let client = $state(untrack(() => reqClient))
  let submitted = $state(false)
  let ruleOpen = $state(false)
  let rulePreset = $state.raw<Partial<FilterRuleInput>>({})

  // Links from the query log change the URL while this tab is open.
  $effect(() => {
    const [n, ty, c] = [reqName, reqType, reqClient]
    untrack(() => {
      domain = n
      qtype = ty
      client = c
    })
  })

  const domainError = $derived(
    fieldError(result.error, 'name') ?? (submitted && !domain.trim() ? t('common.field.required') : undefined),
  )
  const clientError = $derived(fieldError(result.error, 'clientIp'))
  const otherError = $derived(
    result.error && (!result.error.field || result.error.field === 'type') ? errorText(result.error) : undefined,
  )

  const typeOptions = $derived(
    [...TYPES, ...(TYPES.includes(qtype) ? [] : [qtype])].map((q) => ({ value: q, label: q })),
  )

  function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    const name = asciiDomain(domain)
    if (!name) return
    domain = name
    const patch = { domain: name, qtype: qtype === 'A' ? null : qtype, client: client.trim() }
    if (name === reqName && (patch.qtype ?? 'A') === reqType && patch.client === reqClient) void result.refresh()
    else router.setQuery(patch)
  }

  const res = $derived(result.data)
  const blocked = $derived(!!res && isBlockedStatus(res.status))
  // Like the query panel: an allow rule cannot lift an upstream's block, and
  // a dropped query (blocked client or dropped domain) is not a rule matter.
  const ruleAction = $derived(
    !res || res.status === 'blocked-upstream' || res.status === 'dropped' ? undefined : blocked ? 'allow' : 'block',
  )

  function createRule() {
    if (!res || !ruleAction) return
    rulePreset = { action: ruleAction, type: 'exact', pattern: res.name, groupIds: [DEFAULT_GROUP_ID] }
    ruleOpen = true
  }
</script>

<div class="stack">
  <Panel title={t('dns.tester.title')} description={t('dns.tester.description')}>
    <form class="form" onsubmit={submit} novalidate>
      <div class="domain">
        <Field label={t('common.label.domain')} error={domainError} required>
          <Input bind:value={domain} mono placeholder="ads.example.com" maxlength={253} autocomplete="off" />
        </Field>
      </div>
      <div class="type">
        <Field label={t('common.label.type')}>
          <Select bind:value={qtype} options={typeOptions} />
        </Field>
      </div>
      <div class="client">
        <Field label={t('dns.tester.client')} optional error={clientError}>
          <Input bind:value={client} mono placeholder={t('dns.tester.clientPlaceholder')} maxlength={64} autocomplete="off" />
        </Field>
      </div>
      <div class="go">
        <Button type="submit" variant="primary" icon="search" loading={result.loading}>{t('dns.tester.run')}</Button>
      </div>
    </form>
  </Panel>

  {#if otherError}
    <Notice tone="fail">{otherError}</Notice>
  {/if}

  {#if res && reqName}
    <Panel title={t('dns.tester.resultTitle', { name: res.name, type: res.type })}>
      {#snippet actions()}
        {#if ruleAction}
          <Button icon={ruleAction === 'allow' ? 'shield-off' : 'shield'} disabled={!session.isAdmin} onclick={createRule}>
            {ruleAction === 'allow' ? t('dns.queryLog.allowDomain') : t('dns.queryLog.blockDomain')}
          </Button>
        {/if}
        <Button variant="ghost" icon="list" href={href('/dns/queries', { domain: `"${res.name}"` })}>{t('dns.tester.showInLog')}</Button>
      {/snippet}
      <div class="stack">
        <div class="row">
          <QueryStatusChip status={res.status} size="md" />
          <span class="muted small">{res.rcode ? `${res.rcode} · ` : ''}{formatMicros(res.durationUs)}</span>
        </div>
        {#if res.status === 'blocked-upstream'}
          <Notice tone="info" title={t('dns.queryLog.upstreamBlockTitle')}>
            {t('dns.queryLog.upstreamBlockText')}
            {#snippet actions()}
              <Button size="sm" icon="link" href={href('/dns/local', { tab: 'forwarders' })}>{t('dns.queryLog.openForwarders')}</Button>
            {/snippet}
          </Notice>
        {/if}
        <KeyValue
          items={[
            { label: t('dns.queryLog.reason'), value: res.reason },
            { label: t('dns.queryLog.upstream'), value: res.upstream, mono: true },
            { label: t('dns.tester.groups'), value: groupNames(res.groupIds, groups) },
          ]}
        />

        <section class="stack-sm" aria-labelledby="tester-answers">
          <h3 id="tester-answers">{t('dns.tester.answers')}</h3>
          {#if res.answers.length > 0}
            <ul class="answers mono small">
              {#each res.answers as a, i (i)}<li>{a}</li>{/each}
            </ul>
          {:else}
            <p class="small muted">{t('dns.tester.noAnswers')}</p>
          {/if}
        </section>

        <section class="stack-sm" aria-labelledby="tester-matches">
          <h3 id="tester-matches">{t('dns.tester.matchesTitle')}</h3>
          <MatchList matches={res.matches} {groups} />
        </section>

        <section class="stack-sm" aria-labelledby="tester-steps">
          <h3 id="tester-steps">{t('dns.tester.stepsTitle')}</h3>
          <ol class="steps small">
            {#each res.steps as step, i (i)}<li>{step}</li>{/each}
          </ol>
        </section>
      </div>
    </Panel>
  {:else if !reqName && !result.loading}
    <Panel>
      <EmptyState icon="search" title={t('dns.tester.emptyTitle')} text={t('dns.tester.emptyText')} />
    </Panel>
  {/if}
</div>

<RulePanel bind:open={ruleOpen} preset={rulePreset} {groups} onsaved={() => result.refresh()} />

<style>
  .form {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .domain {
    flex: 2 1 260px;
  }
  .type {
    flex: 0 1 120px;
  }
  .client {
    flex: 1 1 200px;
  }
  .go {
    padding-top: 26px;
  }
  h3 {
    font-size: var(--fs-md);
  }
  .answers {
    margin: 0;
    padding: var(--sp-3);
    list-style: none;
    border-radius: var(--r-control);
    background: var(--surface-2);
    overflow-wrap: anywhere;
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding-left: var(--sp-5);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  @media (max-width: 480px) {
    .domain,
    .type,
    .client {
      flex: 1 1 100%;
    }
    .go {
      padding-top: 0;
    }
  }
</style>
