<!--
  @component
  An editable list of upstream DNS servers: one row per entry with its
  health and statistics, a Test button (tests the typed address, saved or
  not; admins may test while the host locks the configuration, so the
  entries are disabled one by one), optional order buttons and "Add". Errors of single entries
  ("dns.upstreams[1]") appear under the entry, errors of the list below it.

  <UpstreamList bind:value={d.upstreams} {form} field="dns.upstreams" max={16} {stats} orderable />
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type UpstreamStat, type UpstreamTestResult } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatNumber, formatRelative } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, IconButton, Input } from '$lib/ui'
  import { lineError } from '../shared/errors'

  interface Props {
    value: string[]
    form: SettingsForm<'dns'>
    /** Settings field of the list, e.g. "dns.upstreams". */
    field: string
    max: number
    /** Live statistics of the saved entries (/dns/upstreams). */
    stats: readonly UpstreamStat[] | undefined
    /** Show move up/down (the order matters). */
    orderable?: boolean
    /** Accessible name of entry n. */
    entryLabel: (n: number) => string
    addLabel: string
    /** Buttons after "Add". */
    extra?: Snippet
  }

  let { value = $bindable(), form, field, max, stats, orderable = false, entryLabel, addLabel, extra }: Props = $props()

  type Test = { running: boolean; result?: UpstreamTestResult; error?: ApiError }
  let tests = $state<Record<string, Test>>({})

  function statFor(u: string): UpstreamStat | undefined {
    return stats?.find((s) => s.upstream === u.trim())
  }

  async function test(u: string) {
    const key = u.trim()
    if (!key) return
    tests[key] = { running: true }
    try {
      tests[key] = { running: false, result: await api.upstreams.test(key) }
    } catch (e) {
      tests[key] = { running: false, error: toApiError(e) }
    }
  }

  function move(i: number, by: number) {
    const list = [...value]
    const j = i + by
    if (j < 0 || j >= list.length) return
    ;[list[i], list[j]] = [list[j], list[i]]
    value = list
  }

  function removeAt(i: number) {
    value = value.filter((_, k) => k !== i)
  }

  function add() {
    if (value.length < max) value = [...value, '']
  }

  /** The list itself (not one entry): none given, too many. */
  const listError = $derived(form.saveError?.field === field ? form.errorMessage : undefined)
</script>

<div class="stack-sm">
  {#if value.length > 0}
    <ol class="ups">
      {#each value as _, i (i)}
        {@const u = value[i]}
        {@const st = statFor(u)}
        {@const tr = tests[u.trim()]}
        {@const err = lineError(form.saveError, `${field}[${i}]`)}
        <li>
          <div class="line">
            <span class="pos num">{i + 1}</span>
            <div class="input">
              <Input
                bind:value={value[i]}
                mono
                invalid={!!err}
                aria-label={entryLabel(i + 1)}
                placeholder="https://dns.example/dns-query"
                maxlength={512}
                disabled={!session.isAdmin}
              />
            </div>
            <Button size="sm" loading={tr?.running} disabled={!session.canOperate || !u.trim()} onclick={() => test(u)}>
              {t('common.action.test')}
            </Button>
            {#if orderable}
              <IconButton icon="arrow-up" size="sm" label={t('dns.settings.upstreams.up')} disabled={!session.isAdmin || i === 0} onclick={() => move(i, -1)} />
              <IconButton
                icon="arrow-down"
                size="sm"
                label={t('dns.settings.upstreams.down')}
                disabled={!session.isAdmin || i === value.length - 1}
                onclick={() => move(i, 1)}
              />
            {/if}
            <IconButton icon="trash" size="sm" variant="danger" label={t('dns.settings.upstreams.remove')} disabled={!session.isAdmin} onclick={() => removeAt(i)} />
          </div>
          {#if err}<p class="err">{err}</p>{/if}
          <div class="meta small">
            {#if st}
              <Chip size="sm" tone={st.healthy ? 'ok' : 'fail'} label={st.healthy ? t('dns.settings.upstreams.healthy') : t('dns.settings.upstreams.failing')} />
              <span class="muted">
                {t('dns.settings.upstreams.stats', {
                  queries: tn('dns.settings.upstreams.queries', st.queries, { count: formatNumber(st.queries) }),
                  errors: tn('dns.settings.upstreams.errors', st.errors, { count: formatNumber(st.errors) }),
                  rtt: formatNumber(st.avgRttMs, 1),
                })}
              </span>
              {#if st.lastError}
                <span class="warn" title={st.lastError}>
                  {t('dns.settings.upstreams.lastError', { when: formatRelative(st.lastErrorAt) })}
                </span>
              {/if}
            {:else if u.trim()}
              <span class="subtle">{t('dns.settings.upstreams.notInUse')}</span>
            {/if}
            {#if tr?.result}
              {#if tr.result.ok}
                <span class="ok">{t('dns.settings.upstreams.testOk', { rtt: formatNumber(tr.result.rttMs, 1) })}</span>
                {#if tr.result.answer}<span class="mono subtle">{tr.result.answer}</span>{/if}
              {:else}
                <span class="bad">{t('dns.settings.upstreams.testFailed', { error: tr.result.error ?? '' })}</span>
              {/if}
            {:else if tr?.error}
              <span class="bad">{errorText(tr.error)}</span>
            {/if}
          </div>
        </li>
      {/each}
    </ol>
  {/if}
  {#if listError}<p class="err">{listError}</p>{/if}
  <div class="row">
    <Button size="sm" icon="plus" disabled={!session.isAdmin || value.length >= max} onclick={add}>{addLabel}</Button>
    {@render extra?.()}
  </div>
</div>

<style>
  .ups {
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
    gap: 4px;
    min-width: 0;
  }
  .line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
  }
  .pos {
    width: 2ch;
    color: var(--text-3);
    font-size: var(--fs-sm);
  }
  .input {
    flex: 1 1 260px;
    min-width: 0;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
    padding-left: calc(2ch + var(--sp-2));
    min-width: 0;
  }
  .meta .mono {
    overflow-wrap: anywhere;
  }
  .err {
    padding-left: calc(2ch + var(--sp-2));
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .ok {
    color: var(--ok);
  }
  .bad {
    color: var(--danger);
  }
  .warn {
    color: var(--warning);
  }
</style>
