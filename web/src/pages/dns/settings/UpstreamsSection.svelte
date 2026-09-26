<!--
  @component
  Upstream DNS servers: one row per upstream with its health and a Test
  button (tests the typed address, saved or not), order (strict mode uses
  it), the fallback servers asked only when every upstream fails to answer,
  mode, timeout, bootstrap servers (IPv6 first optionally), private
  reverse-lookup servers and the EDNS Client Subnet sent upstream.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DnsSettings, EcsSettings, Timestamp, UpstreamMode, UpstreamStat } from '$lib/api'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Button, Field, Input, Notice, Panel, Select, Toggle } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'
  import NumberInput from '../shared/NumberInput.svelte'
  import UpstreamList from './UpstreamList.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    /** Live upstream statistics (/dns/upstreams). */
    stats: readonly UpstreamStat[] | undefined
    /** Live statistics of the fallback servers. */
    fallbackStats: readonly UpstreamStat[] | undefined
    /** When a fallback server last answered (absent if never since the start). */
    fallbackLastUsed: Timestamp | undefined
    clockGuard: boolean
  }

  let { form, stats, fallbackStats, fallbackLastUsed, clockGuard }: Props = $props()

  const MAX = 16
  const MAX_FALLBACKS = 4
  const MODES: UpstreamMode[] = ['load_balance', 'parallel', 'strict', 'fastest_addr']
  const ECS_MODES: EcsSettings['mode'][] = ['off', 'client', 'custom']

  const d = $derived(form.draft as DnsSettings)

  /** A fallback answered within the last 5 minutes (the health check warns then too). */
  function recent(ts: Timestamp): boolean {
    return Date.now() - new Date(ts).getTime() < 5 * 60_000
  }

  const defaultServers = $derived(form.isDefault('upstreams') && form.isDefault('fallbackUpstreams'))

  function useDefaults() {
    form.resetToDefault('upstreams')
    form.resetToDefault('fallbackUpstreams')
  }

  const modeOptions = $derived([
    { value: 'load_balance', label: t('dns.settings.mode.load_balance') },
    { value: 'parallel', label: t('dns.settings.mode.parallel') },
    { value: 'strict', label: t('dns.settings.mode.strict') },
    { value: 'fastest_addr', label: t('dns.settings.mode.fastest_addr') },
  ])
  const modeHelp = $derived(
    {
      load_balance: t('dns.settings.modeHelp.load_balance'),
      parallel: t('dns.settings.modeHelp.parallel'),
      strict: t('dns.settings.modeHelp.strict'),
      fastest_addr: t('dns.settings.modeHelp.fastest_addr'),
    }[d.upstreamMode] ?? t('dns.settings.modeHelp.load_balance'),
  )

  const ecsOptions = $derived([
    { value: 'off', label: t('dns.settings.ecs.mode.off') },
    { value: 'client', label: t('dns.settings.ecs.mode.client') },
    { value: 'custom', label: t('dns.settings.ecs.mode.custom') },
  ])
</script>

<Panel id="dns-set-upstreams" title={t('dns.settings.upstreams.title')} description={t('dns.settings.upstreams.description')}>
  <div class="stack">
    {#if clockGuard}
      <Notice tone="warn" title={t('dns.settings.clockGuardTitle')}>{t('dns.settings.clockGuardText')}</Notice>
    {/if}

    <fieldset class="stack-sm">
      <legend class="visually-hidden">{t('dns.settings.upstreams.title')}</legend>
      <UpstreamList
        bind:value={d.upstreams}
        {form}
        field="dns.upstreams"
        max={MAX}
        {stats}
        orderable
        entryLabel={(n) => t('dns.settings.upstreams.entry', { n })}
        addLabel={t('dns.settings.upstreams.add')}
      >
        {#snippet extra()}
          {#if !defaultServers}
            <span class="long">
              <Button size="sm" variant="ghost" disabled={!session.isAdmin} onclick={useDefaults}>{t('dns.settings.upstreams.useDefaults')}</Button>
            </span>
          {/if}
        {/snippet}
      </UpstreamList>
      <p class="small muted">{t('dns.settings.upstreams.syntax')}</p>
    </fieldset>

    <section class="stack-sm sub" aria-labelledby="dns-fallback-title">
      <h3 id="dns-fallback-title">{t('dns.settings.fallback.title')}</h3>
      <p class="small muted">{t('dns.settings.fallback.help')}</p>
      {#if d.fallbackUpstreams.length > 0}
        <p class="small">
          {#if fallbackLastUsed}
            <span class={recent(fallbackLastUsed) ? 'warn' : 'muted'} title={formatDateTime(fallbackLastUsed)}>
              {t('dns.settings.fallback.lastUsed', { when: formatRelative(fallbackLastUsed) })}
            </span>
          {:else}
            <span class="subtle">{t('dns.settings.fallback.neverUsed')}</span>
          {/if}
        </p>
      {/if}
      <UpstreamList
        bind:value={d.fallbackUpstreams}
        {form}
        field="dns.fallbackUpstreams"
        max={MAX_FALLBACKS}
        stats={fallbackStats}
        entryLabel={(n) => t('dns.settings.fallback.entry', { n })}
        addLabel={t('dns.settings.fallback.add')}
      />
      {#if d.fallbackUpstreams.length === 0}<p class="small subtle">{t('dns.settings.fallback.none')}</p>{/if}
    </section>

    <!-- The upstream lists disable their entries themselves (their Test buttons stay usable). -->
    <fieldset class="stack" disabled={!session.isAdmin}>
      <div class="grid">
        <Field label={t('dns.settings.mode')} help={modeHelp} error={form.error('upstreamMode')}>
          <Select
            bind:value={() => d.upstreamMode, (v) => (d.upstreamMode = MODES.includes(v as UpstreamMode) ? (v as UpstreamMode) : 'load_balance')}
            options={modeOptions}
          />
        </Field>
        <Field label={t('dns.settings.timeout')} help={t('dns.settings.timeoutHelp')} error={form.error('upstreamTimeoutMs')}>
          <NumberInput bind:value={d.upstreamTimeoutMs} min={500} max={60000} unit={t('dns.shared.unit.ms')} />
        </Field>
      </div>

      <div class="grid">
        <div class="stack-sm">
          <Field label={t('dns.settings.bootstrap')} help={t('dns.settings.bootstrapHelp')} error={lineError(form.saveError, 'dns.bootstrap')}>
            <LinesInput bind:value={d.bootstrap} rows={6} placeholder="9.9.9.9" />
          </Field>
          <Toggle
            bind:checked={d.bootstrapPreferIpv6}
            label={t('dns.settings.preferIpv6')}
            description={t('dns.settings.preferIpv6Help')}
          />
          {#if form.error('bootstrapPreferIpv6')}<p class="err">{form.error('bootstrapPreferIpv6')}</p>{/if}
        </div>
        <Field label={t('dns.settings.localPtr')} optional help={t('dns.settings.localPtrHelp')} error={lineError(form.saveError, 'dns.localPtrUpstreams')}>
          <LinesInput bind:value={d.localPtrUpstreams} rows={4} placeholder="192.168.1.1" />
        </Field>
      </div>

      <section class="stack-sm sub" aria-labelledby="dns-ecs-title">
        <h3 id="dns-ecs-title">{t('dns.settings.ecs.title')}</h3>
        <p class="small muted">{t('dns.settings.ecs.help')}</p>
        <div class="grid">
          <Field label={t('dns.settings.ecs.mode')} error={form.error('ecs.mode')}>
            <Select
              bind:value={() => d.ecs.mode, (v) => (d.ecs.mode = ECS_MODES.includes(v as EcsSettings['mode']) ? (v as EcsSettings['mode']) : 'off')}
              options={ecsOptions}
            />
          </Field>
          {#if d.ecs.mode === 'custom' || d.ecs.customSubnet}
            <Field
              label={t('dns.settings.ecs.subnet')}
              required={d.ecs.mode === 'custom'}
              help={t('dns.settings.ecs.subnetHelp')}
              error={form.error('ecs.customSubnet')}
            >
              <Input bind:value={d.ecs.customSubnet} mono placeholder="203.0.113.0/24" maxlength={64} autocomplete="off" />
            </Field>
          {/if}
        </div>
        {#if d.ecs.mode === 'client'}
          <Notice tone="warn">{t('dns.settings.ecs.clientWarn')}</Notice>
        {/if}
      </section>
    </fieldset>
  </div>
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  /* Long in German: wraps on phones instead of leaving the panel. */
  .long {
    min-width: 0;
    max-width: 100%;
  }
  .long :global(.btn) {
    max-width: 100%;
    height: auto;
    min-height: var(--control-h-sm);
    padding-block: 6px;
    line-height: 1.3;
    white-space: normal;
    text-align: left;
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
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
