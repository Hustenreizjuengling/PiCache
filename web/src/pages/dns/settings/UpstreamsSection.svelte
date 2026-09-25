<!--
  @component
  Upstream DNS servers: one row per upstream with its health and a Test
  button (tests the typed address, saved or not), order (strict mode uses
  it), mode, timeout, bootstrap servers and private reverse-lookup servers.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type DnsSettings, type UpstreamStat, type UpstreamTestResult } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatNumber, formatRelative } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, Field, IconButton, Input, Notice, Panel, Select } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'
  import NumberInput from '../shared/NumberInput.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    /** Live upstream statistics (/dns/upstreams). */
    stats: readonly UpstreamStat[] | undefined
    clockGuard: boolean
  }

  let { form, stats, clockGuard }: Props = $props()

  const MAX = 16
  const d = $derived(form.draft as DnsSettings)

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
    const list = [...d.upstreams]
    const j = i + by
    if (j < 0 || j >= list.length) return
    ;[list[i], list[j]] = [list[j], list[i]]
    d.upstreams = list
  }

  function removeAt(i: number) {
    d.upstreams = d.upstreams.filter((_, k) => k !== i)
  }

  function add() {
    if (d.upstreams.length < MAX) d.upstreams = [...d.upstreams, '']
  }

  /** "dns.upstreams" itself (not one entry): none given, too many. */
  const listError = $derived(form.saveError?.field === 'dns.upstreams' ? form.errorMessage : undefined)

  const modeOptions = $derived([
    { value: 'load_balance', label: t('dns.settings.mode.load_balance') },
    { value: 'parallel', label: t('dns.settings.mode.parallel') },
    { value: 'strict', label: t('dns.settings.mode.strict') },
  ])
  const modeHelp = $derived(
    d.upstreamMode === 'parallel'
      ? t('dns.settings.modeHelp.parallel')
      : d.upstreamMode === 'strict'
        ? t('dns.settings.modeHelp.strict')
        : t('dns.settings.modeHelp.load_balance'),
  )
</script>

<Panel id="dns-set-upstreams" title={t('dns.settings.upstreams.title')} description={t('dns.settings.upstreams.description')}>
  <div class="stack">
    {#if clockGuard}
      <Notice tone="warn" title={t('dns.settings.clockGuardTitle')}>{t('dns.settings.clockGuardText')}</Notice>
    {/if}

    <fieldset class="stack-sm">
      <legend class="visually-hidden">{t('dns.settings.upstreams.title')}</legend>
      <ol class="ups">
        {#each d.upstreams as _, i (i)}
          {@const u = d.upstreams[i]}
          {@const st = statFor(u)}
          {@const tr = tests[u.trim()]}
          {@const err = lineError(form.saveError, `dns.upstreams[${i}]`)}
          <li>
            <div class="line">
              <span class="pos num">{i + 1}</span>
              <div class="input">
                <Input
                  bind:value={d.upstreams[i]}
                  mono
                  invalid={!!err}
                  aria-label={t('dns.settings.upstreams.entry', { n: i + 1 })}
                  placeholder="https://dns.example/dns-query"
                  maxlength={512}
                />
              </div>
              <Button size="sm" loading={tr?.running} disabled={!session.isAdmin || !u.trim()} onclick={() => test(u)}>
                {t('common.action.test')}
              </Button>
              <IconButton icon="arrow-up" size="sm" label={t('dns.settings.upstreams.up')} disabled={i === 0} onclick={() => move(i, -1)} />
              <IconButton
                icon="arrow-down"
                size="sm"
                label={t('dns.settings.upstreams.down')}
                disabled={i === d.upstreams.length - 1}
                onclick={() => move(i, 1)}
              />
              <IconButton icon="trash" size="sm" variant="danger" label={t('dns.settings.upstreams.remove')} onclick={() => removeAt(i)} />
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
      {#if listError}<p class="err">{listError}</p>{/if}
      <div class="row">
        <Button size="sm" icon="plus" disabled={d.upstreams.length >= MAX} onclick={add}>{t('dns.settings.upstreams.add')}</Button>
        {#if !form.isDefault('upstreams')}
          <Button size="sm" variant="ghost" onclick={() => form.resetToDefault('upstreams')}>{t('dns.settings.upstreams.useDefaults')}</Button>
        {/if}
      </div>
      <p class="small muted">{t('dns.settings.upstreams.syntax')}</p>
    </fieldset>

    <div class="grid">
      <Field label={t('dns.settings.mode')} help={modeHelp} error={form.error('upstreamMode')}>
        <Select bind:value={() => d.upstreamMode, (v) => (d.upstreamMode = v as DnsSettings['upstreamMode'])} options={modeOptions} />
      </Field>
      <Field label={t('dns.settings.timeout')} help={t('dns.settings.timeoutHelp')} error={form.error('upstreamTimeoutMs')}>
        <NumberInput bind:value={d.upstreamTimeoutMs} min={500} max={60000} unit={t('dns.shared.unit.ms')} />
      </Field>
    </div>

    <div class="grid">
      <Field label={t('dns.settings.bootstrap')} help={t('dns.settings.bootstrapHelp')} error={lineError(form.saveError, 'dns.bootstrap')}>
        <LinesInput bind:value={d.bootstrap} rows={4} placeholder="9.9.9.9" />
      </Field>
      <Field label={t('dns.settings.localPtr')} optional help={t('dns.settings.localPtrHelp')} error={lineError(form.saveError, 'dns.localPtrUpstreams')}>
        <LinesInput bind:value={d.localPtrUpstreams} rows={4} placeholder="192.168.1.1" />
      </Field>
    </div>
  </div>
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
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
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  @media (max-width: 700px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
