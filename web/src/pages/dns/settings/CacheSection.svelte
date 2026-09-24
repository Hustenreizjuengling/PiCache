<!--
  @component
  DNS response cache: on/off, size, TTL limits and serve-stale, with the
  live hit rate and "Flush DNS cache".
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type DnsSettings, type UpstreamCacheStat } from '$lib/api'
  import { formatNumber, formatPercent } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Field, Panel, toast, Toggle } from '$lib/ui'
  import NumberInput from '../shared/NumberInput.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    cache: UpstreamCacheStat | undefined
    onflushed: () => void
  }

  let { form, cache, onflushed }: Props = $props()

  const d = $derived(form.draft as DnsSettings)
  let flushing = $state(false)

  const hitRate = $derived(cache && cache.hits + cache.misses > 0 ? cache.hits / (cache.hits + cache.misses) : undefined)

  async function flush() {
    flushing = true
    try {
      await api.upstreams.flushCache()
      toast.success(t('dns.settings.cache.flushed'))
      onflushed()
    } catch (e) {
      toast.error(e)
    } finally {
      flushing = false
    }
  }
</script>

<Panel id="dns-set-cache" title={t('dns.settings.cache.title')} description={t('dns.settings.cache.description')}>
  {#snippet actions()}
    <Button size="sm" icon="refresh" loading={flushing} disabled={!session.isAdmin} onclick={flush}>{t('dns.settings.cache.flush')}</Button>
  {/snippet}
  <div class="stack">
    {#if cache}
      <p class="small muted">
        {t('dns.settings.cache.stats', {
          entries: formatNumber(cache.entries),
          capacity: formatNumber(cache.capacity),
          hitRate: formatPercent(hitRate),
          stale: formatNumber(cache.staleHits),
        })}
      </p>
    {/if}
    <Toggle bind:checked={d.cacheEnabled} label={t('dns.settings.cache.enabled')} description={t('dns.settings.cache.enabledHelp')} />
    {#if d.cacheEnabled}
      <div class="grid">
        <Field label={t('dns.settings.cache.size')} error={form.error('cacheSize')}>
          <NumberInput bind:value={d.cacheSize} min={0} max={10000000} unit={t('dns.shared.unit.entries')} />
        </Field>
        <Field label={t('dns.settings.cache.minTtl')} help={t('dns.settings.cache.minTtlHelp')} error={form.error('cacheMinTtl')}>
          <NumberInput bind:value={d.cacheMinTtl} min={0} max={86400} unit={t('dns.shared.unit.seconds')} />
        </Field>
        <Field label={t('dns.settings.cache.maxTtl')} help={t('dns.settings.cache.maxTtlHelp')} error={form.error('cacheMaxTtl')}>
          <NumberInput bind:value={d.cacheMaxTtl} min={0} max={604800} unit={t('dns.shared.unit.seconds')} />
        </Field>
      </div>
      <Toggle bind:checked={d.serveStale} label={t('dns.settings.cache.serveStale')} description={t('dns.settings.cache.serveStaleHelp')} />
      {#if d.serveStale}
        <Field label={t('dns.settings.cache.staleAge')} error={form.error('serveStaleMaxAgeSec')}>
          <NumberInput bind:value={d.serveStaleMaxAgeSec} min={0} max={604800} unit={t('dns.shared.unit.seconds')} />
        </Field>
      {/if}
    {/if}
  </div>
</Panel>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  @media (max-width: 900px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
