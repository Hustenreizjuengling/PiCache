<!--
  @component
  DNS settings: upstreams (with tests, health and fallbacks), response cache,
  blocking replies and special domains, protection (rebinding, bogus
  NXDOMAIN, dropped domains), rate limiting, access (blocked clients,
  trusted forwarders), local names, IPv6 answers (no AAAA, DNS64) and DNSSEC. Edits the "dns" and "filter"
  settings sections; Save sends only the changed members of each.
  Validation errors appear next to the field.
  Query: ?section=upstreams|cache|blocking|protection|ratelimit|access|names|ipv6|dnssec (scrolls there)
-->
<script lang="ts">
  import { tick, untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Button, Notice, Skeleton, toast } from '$lib/ui'
  import AccessSection from './settings/AccessSection.svelte'
  import BlockingSection from './settings/BlockingSection.svelte'
  import CacheSection from './settings/CacheSection.svelte'
  import DnssecSection from './settings/DnssecSection.svelte'
  import Ipv6Section from './settings/Ipv6Section.svelte'
  import NamesSection from './settings/NamesSection.svelte'
  import ProtectionSection from './settings/ProtectionSection.svelte'
  import RateLimitSection from './settings/RateLimitSection.svelte'
  import UpstreamsSection from './settings/UpstreamsSection.svelte'

  const dns = settingsForm('dns')
  const filter = settingsForm('filter')
  const upstreams = resource((signal) => api.upstreams.get({ signal }), { interval: 10_000 })
  const dnsStats = resource((signal) => api.dns.stats({ signal }), { interval: 10_000 })
  // Names of the groups and presets of the group upstream lists.
  const groups = resource((signal) => api.groups.list({ signal }))
  const presets = resource((signal) => api.groups.upstreamPresets({ signal }))

  const SECTIONS = ['upstreams', 'cache', 'blocking', 'protection', 'ratelimit', 'access', 'names', 'ipv6', 'dnssec'] as const
  type Section = (typeof SECTIONS)[number]

  const ready = $derived(!!dns.draft && !!filter.draft)
  const dirty = $derived(dns.dirty || filter.dirty)
  const saving = $derived(dns.saving || filter.saving)
  const loadError = $derived(dns.loadError ?? filter.loadError)
  const saveErrors = $derived([dns.errorMessage, filter.errorMessage].filter((m): m is string => !!m))

  let root: HTMLElement | undefined = $state()

  function sectionLabel(s: Section): string {
    return {
      upstreams: t('dns.settings.upstreams.title'),
      cache: t('dns.settings.cache.title'),
      blocking: t('dns.settings.blocking.title'),
      protection: t('dns.settings.protection.title'),
      ratelimit: t('dns.settings.rate.title'),
      access: t('dns.settings.access.title'),
      names: t('dns.settings.names.title'),
      ipv6: t('dns.settings.ipv6.title'),
      dnssec: t('dns.settings.dnssec.title'),
    }[s]
  }

  function jump(s: Section) {
    document.getElementById(`dns-set-${s}`)?.scrollIntoView({ block: 'start' })
  }

  // ?section=… (e.g. from Local DNS) scrolls to that section once the page
  // and the statistics above it are loaded (so nothing pushes it away).
  const section = $derived(router.param('section') as Section)
  const settled = $derived(
    ready && (upstreams.loaded || !!upstreams.error) && (dnsStats.loaded || !!dnsStats.error),
  )
  $effect(() => {
    const s = section
    if (!settled || !SECTIONS.includes(s)) return
    untrack(() => void tick().then(() => jump(s)))
  })

  async function save() {
    const okDns = await dns.save()
    const okFilter = await filter.save()
    if (okDns && okFilter) {
      toast.success(t('common.state.saved'))
      return
    }
    await tick()
    root?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }

  function revert() {
    dns.revert()
    filter.revert()
  }
</script>

<div class="page" bind:this={root}>
  {#if loadError && !ready}
    <Notice tone="fail" title={t('dns.settings.loadError')}>
      {errorText(loadError)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => Promise.all([dns.load(), filter.load()])}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !ready}
    <Skeleton height="240px" />
    <Skeleton height="180px" />
  {:else}
    <nav class="jump" aria-label={t('dns.settings.jumpLabel')}>
      {#each SECTIONS as s (s)}
        <button type="button" class="jump-link" onclick={() => jump(s)}>{sectionLabel(s)}</button>
      {/each}
    </nav>

    {#if !session.canOperate}<Notice>{t('common.state.readOnly')}</Notice>{/if}

    <!-- Upstream tests and the cache flush stay usable for admins while the host
         locks the configuration: those two sections disable their settings themselves. -->
    <fieldset class="sections" disabled={!session.canOperate}>
      <UpstreamsSection
        form={dns}
        stats={upstreams.data?.upstreams}
        fallbackStats={upstreams.data?.fallbacks}
        fallbackLastUsed={upstreams.data?.fallbackLastUsed}
        clockGuard={!!upstreams.data?.clockGuard}
        groupSets={upstreams.data?.groups}
        groups={groups.data}
        presets={presets.data}
      />
      <CacheSection form={dns} cache={upstreams.data?.cache} onflushed={() => upstreams.refresh()} />
      <fieldset class="sections" disabled={!session.isAdmin}>
        <BlockingSection form={filter} />
        <ProtectionSection form={dns} stats={dnsStats.data} />
        <RateLimitSection form={dns} stats={dnsStats.data} />
        <AccessSection form={dns} stats={dnsStats.data} />
        <NamesSection form={dns} status={appStatus.overview.data?.router} />
        <Ipv6Section form={dns} />
        <DnssecSection form={dns} />
      </fieldset>
    </fieldset>

    {#if dirty || saveErrors.length > 0}
      <!-- Buttons first: toasts appear at the bottom right and must not cover them. -->
      <div class="savebar" role="region" aria-label={t('dns.settings.saveBar')}>
        <Button variant="primary" loading={saving} disabled={!session.isAdmin || !dirty} onclick={save}>{t('common.action.save')}</Button>
        <Button variant="ghost" disabled={saving || !dirty} onclick={revert}>{t('dns.settings.discard')}</Button>
        <div class="msg">
          {#if saveErrors.length > 0}
            {#each saveErrors as m, i (i)}<p class="err">{m}</p>{/each}
          {:else}
            <p>{t('dns.settings.unsaved')}</p>
          {/if}
        </div>
      </div>
    {/if}
  {/if}
</div>

<style>
  .jump {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-3);
  }
  .jump-link {
    padding: 2px 0;
    border: 0;
    background: none;
    color: var(--text-2);
    font: inherit;
    font-size: var(--fs-sm);
    text-decoration: underline;
    text-underline-offset: 3px;
    cursor: pointer;
  }
  .jump-link:hover {
    color: var(--text);
  }
  .jump-link:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .sections {
    display: flex;
    flex-direction: column;
    gap: var(--sp-5);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .sections :global(section[id^='dns-set-']) {
    scroll-margin-top: var(--sp-4);
  }
  .savebar {
    position: sticky;
    bottom: 0;
    z-index: 5;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
    box-shadow: var(--shadow-float);
  }
  .msg {
    flex: 1 1 240px;
    min-width: 0;
    font-size: var(--fs-sm);
  }
  .err {
    color: var(--danger);
    overflow-wrap: anywhere;
  }
</style>
