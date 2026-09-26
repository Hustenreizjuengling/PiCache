<!--
  @component
  Protection against answers of the public upstreams: DNS rebinding
  protection (private addresses in public answers are blocked) with its
  allow list and a hint for local resolvers used as upstreams, bogus
  NXDOMAIN addresses, dropped domains (no answer, not logged) and how long
  answers the upstream blocked are kept.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { DnsSettings, DnsStats } from '$lib/api'
  import { formatNumber } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Button, Field, Notice, Panel, Toggle } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'
  import NumberInput from '../shared/NumberInput.svelte'
  import { localUpstreams } from './ranges'

  interface Props {
    form: SettingsForm<'dns'>
    stats: DnsStats | undefined
  }

  let { form, stats }: Props = $props()

  const d = $derived(form.draft as DnsSettings)

  // Local resolvers used as upstreams answer with private addresses on purpose.
  const local = $derived(d.rebindProtection ? localUpstreams([...d.upstreams, ...d.fallbackUpstreams]) : [])
</script>

<Panel id="dns-set-protection" title={t('dns.settings.protection.title')} description={t('dns.settings.protection.description')}>
  <div class="stack">
    <div class="stack-sm">
      <Toggle bind:checked={d.rebindProtection} label={t('dns.settings.protection.rebind')} description={t('dns.settings.protection.rebindHelp')} />
      {#if form.error('rebindProtection')}<p class="err">{form.error('rebindProtection')}</p>{/if}
    </div>

    {#if d.rebindProtection}
      {#if local.length > 0}
        <Notice tone="warn">
          {t('dns.settings.protection.localUpstream', { upstream: local.join(', ') })}
          {#snippet actions()}
            <Button size="sm" icon="link" href={href('/dns/local', { tab: 'forwarders' })}>{t('dns.settings.protection.openForwarders')}</Button>
          {/snippet}
        </Notice>
      {/if}
      <Field
        label={t('dns.settings.protection.allow')}
        optional
        help={t('dns.settings.protection.allowHelp')}
        error={lineError(form.saveError, 'dns.rebindAllow')}
      >
        <LinesInput bind:value={d.rebindAllow} rows={3} placeholder="plex.direct" />
      </Field>
    {/if}

    <div class="grid">
      <Field
        label={t('dns.settings.protection.bogus')}
        optional
        help={t('dns.settings.protection.bogusHelp')}
        error={lineError(form.saveError, 'dns.bogusNxdomain')}
      >
        <LinesInput bind:value={d.bogusNxdomain} rows={3} placeholder="198.51.100.7" />
      </Field>
      <div class="stack-sm">
        <Field
          label={t('dns.settings.protection.dropped')}
          optional
          help={t('dns.settings.protection.droppedHelp')}
          error={lineError(form.saveError, 'dns.droppedDomains')}
        >
          <LinesInput bind:value={d.droppedDomains} rows={3} placeholder={'broken.example\nexample.net:ANY'} />
        </Field>
        {#if stats && stats.dropped > 0}
          <p class="small muted">{tn('dns.settings.protection.droppedCount', stats.dropped, { count: formatNumber(stats.dropped) })}</p>
        {/if}
      </div>
    </div>

    <Field label={t('dns.settings.protection.blockedTtl')} help={t('dns.settings.protection.blockedTtlHelp')} error={form.error('upstreamBlockedTtl')}>
      <NumberInput bind:value={d.upstreamBlockedTtl} min={10} max={86400} unit={t('dns.shared.unit.seconds')} />
    </Field>
  </div>
</Panel>

<style>
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
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
