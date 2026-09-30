<!--
  @component
  DNSSEC: the mode (off; pass the upstreams' verdict, the AD flag, on;
  validate the signatures in PiCache) with the help of the selected mode.
  While the saved mode is validate: what keeps validation from working
  (suspended time checks, upstreams without DNSSEC data, trust anchors that
  no longer match the root zone, a new root key) and the zones in the key
  cache. "Test DNSSEC" works in every mode and stays usable for admins while
  the host locks the configuration or the settings are synced (it stores
  nothing), so the mode has its own fieldset.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { DnsSettings, UpstreamsState, UpstreamStat } from '$lib/api'
  import { formatBytes, formatNumber } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { KeyValue, Notice, Panel, Segmented } from '$lib/ui'
  import { dnssecModeLabel, DNSSEC_MODES, isDnssecMode, timeReasonText } from '../shared/dnssec'
  import DnssecTest from './DnssecTest.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    /** GET /dns/upstreams: the probe states, the time checks and the key cache (validate mode). */
    upstreams: UpstreamsState | undefined
    /** The page has unsaved changes: the test uses the saved settings. */
    dirty: boolean
  }

  let { form, upstreams, dirty }: Props = $props()

  const d = $derived(form.draft as DnsSettings)
  const modeOptions = $derived(DNSSEC_MODES.map((m) => ({ value: m, label: dnssecModeLabel(m) })))
  const modeHelp = $derived(
    {
      off: t('dns.settings.dnssec.modeHelp.off'),
      passthrough: t('dns.settings.dnssec.modeHelp.passthrough'),
      validate: t('dns.settings.dnssec.modeHelp.validate'),
    }[d.dnssecMode] ?? '',
  )

  // What the server does now: the saved mode, not the draft.
  const ds = $derived(form.saved?.dnssecMode === 'validate' ? upstreams?.dnssec : undefined)
  const validation = $derived(form.saved?.dnssecMode === 'validate' ? upstreams?.cache.validation : undefined)

  function label(u: UpstreamStat): string {
    return u.name || u.upstream
  }

  /** The upstreams of the validated routes the health check names: default, fallbacks, validating forwarders. */
  const named = $derived.by(() => {
    if (!upstreams || !ds) return []
    const out: { label: string; stat: UpstreamStat }[] = [
      ...upstreams.upstreams.map((u) => ({ label: label(u), stat: u })),
      ...upstreams.fallbacks.map((u) => ({ label: label(u), stat: u })),
    ]
    for (const f of ds.forwarders) {
      for (const u of f.upstreams) {
        out.push({ label: t('dns.settings.dnssec.forwarderUpstream', { domain: f.domains[0] ?? '', upstream: label(u) }), stat: u })
      }
    }
    return out
  })
  // Once per name (an upstream may also be a fallback).
  const noData = $derived([...new Map(named.filter((n) => n.stat.dnssec === 'no-dnssec').map((n) => [n.label, n])).values()])
  // A mismatch concerns every route (the anchors are the same), group lists included.
  const mismatch = $derived([
    ...new Set([
      ...named.filter((n) => n.stat.dnssec === 'anchor-mismatch').map((n) => n.label),
      ...(ds ? (upstreams?.groups ?? []).flatMap((g) => g.upstreams.filter((u) => u.dnssec === 'anchor-mismatch').map(label)) : []),
    ]),
  ])
</script>

<Panel id="dns-set-dnssec" title={t('dns.settings.dnssec.title')} description={t('dns.settings.dnssec.description')}>
  <div class="stack">
    <fieldset disabled={!session.canEditSection('dns-settings')}>
      <legend class="label">{t('dns.settings.dnssec.mode')}</legend>
      <div class="stack-sm">
        <Segmented
          id="dns-field-dnssecMode"
          label={t('dns.settings.dnssec.mode')}
          describedby="dns-dnssec-mode-help"
          value={d.dnssecMode}
          options={modeOptions}
          onchange={(v) => session.canEditSection('dns-settings') && isDnssecMode(v) && (d.dnssecMode = v)}
        />
        <p id="dns-dnssec-mode-help" class="small muted help">{modeHelp}</p>
        {#if form.error('dnssecMode')}<p class="err">{form.error('dnssecMode')}</p>{/if}
      </div>
    </fieldset>

    {#if ds}
      {#if mismatch.length > 0}
        <Notice tone="fail" title={t('dns.settings.dnssec.anchorMismatchTitle')}>
          {t('dns.settings.dnssec.anchorMismatchText', {
            upstreams: mismatch.length <= 3 ? mismatch.join(', ') : tn('dns.settings.dnssec.upstreamCount', mismatch.length),
          })}
        </Notice>
      {/if}
      {#if ds.timeChecks === 'suspended'}
        <Notice tone="warn" title={t('dns.settings.dnssec.suspendedTitle')}>
          {timeReasonText(ds.timeReason)}
          {t('dns.settings.dnssec.suspendedText')}
        </Notice>
      {/if}
      {#each noData as n (n.label)}
        <Notice tone="warn" title={t('dns.settings.dnssec.noDataTitle', { upstream: n.label })}>
          {t('dns.settings.dnssec.noDataText')}
          {#if n.stat.dnssecError}<span class="mono detail">{n.stat.dnssecError}</span>{/if}
        </Notice>
      {/each}
      {#if ds.newRootKey}
        <Notice tone="warn" title={t('dns.settings.dnssec.newRootKeyTitle')}>{t('dns.settings.dnssec.newRootKeyText')}</Notice>
      {/if}
    {/if}

    {#if validation}
      <section class="stack-sm" aria-labelledby="dns-dnssec-cache">
        <h3 id="dns-dnssec-cache">{t('dns.settings.dnssec.cacheTitle')}</h3>
        <KeyValue
          items={[
            { label: t('dns.settings.dnssec.zones'), value: formatNumber(validation.zones) },
            { label: t('dns.settings.dnssec.zonesSize'), value: formatBytes(validation.bytes) },
            { label: t('dns.settings.dnssec.failures'), value: formatNumber(validation.failures) },
          ]}
        />
      </section>
    {/if}

    {#if session.canOperate}
      <DnssecTest {dirty} />
    {/if}
  </div>
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .label {
    margin-bottom: var(--sp-2);
    padding: 0;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .help {
    max-width: 80ch;
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .detail {
    display: block;
    margin-top: var(--sp-1);
  }
  h3 {
    font-size: var(--fs-md);
  }
</style>
