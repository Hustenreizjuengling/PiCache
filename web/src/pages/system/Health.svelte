<!--
  @component
  Health & about: the health checks with hints, the warning history, host
  resources with their warning thresholds, database sizes, then what is
  running: version and build, uptime, listeners (including failed ones),
  directories, the master key source, memory, the support bundle (admins)
  and links to the documentation.
  Query: ?section=warnings|host|databases|thresholds|support (scrolls there), ?warnings=open
-->
<script lang="ts">
  import { tick, untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatDuration, formatNumber } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Icon, KeyValue, Notice, Panel, Skeleton, type KeyValueItem } from '$lib/ui'
  import RestartButton from './RestartButton.svelte'
  import ChecksPanel from './health/ChecksPanel.svelte'
  import DatabasesPanel from './health/DatabasesPanel.svelte'
  import HostPanel from './health/HostPanel.svelte'
  import ListenersPanel from './health/ListenersPanel.svelte'
  import SupportBundle from './health/SupportBundle.svelte'
  import ThresholdsPanel from './health/ThresholdsPanel.svelte'
  import WarningsPanel from './health/WarningsPanel.svelte'
  import { DOCS, masterKeyKind } from './health/about'

  const health = resource((signal) => api.system.health({ signal }), { interval: 60_000 })
  const info = resource((signal) => api.system.info({ signal }), { interval: 60_000 })
  const host = resource((signal) => api.system.host({ signal }), { interval: 60_000 })
  const databases = resource((signal) => api.system.databases({ signal }), { interval: 60_000 })
  const thresholds = settingsForm('health')

  // ?section=… (e.g. the warnings badge in the top bar) scrolls there once the panels above have loaded.
  const SECTIONS = ['warnings', 'host', 'databases', 'thresholds', 'support']
  const section = $derived(router.param('section'))
  const settled = $derived((health.loaded || !!health.error) && (host.loaded || !!host.error))
  $effect(() => {
    const s = section
    if (!settled || !SECTIONS.includes(s)) return
    untrack(() => void tick().then(() => document.getElementById(s)?.scrollIntoView({ block: 'start' })))
  })

  function buildDate(s: string): string {
    return Number.isNaN(Date.parse(s)) ? s : formatDateTime(s)
  }

  const about = $derived.by((): KeyValueItem[] => {
    const i = info.data
    if (!i) return []
    const v = i.version
    const upd = appStatus.update.data
    return [
      { label: t('system.health.about.version'), value: v.version, mono: true },
      ...(upd?.updateAvailable && upd.latest && upd.status?.state !== 'running'
        ? [
            {
              label: t('system.health.about.update'),
              value: t('system.health.about.updateAvailable', { version: upd.latest.version }),
              href: href('/system/updates'),
            },
          ]
        : []),
      { label: t('system.health.about.commit'), value: v.commit, mono: true },
      { label: t('system.health.about.built'), value: buildDate(v.date) },
      { label: t('system.health.about.go'), value: v.goVersion, mono: true },
      { label: t('system.health.about.platform'), value: `${v.os}/${v.arch}`, mono: true },
      { label: t('system.health.about.started'), value: formatDateTime(i.startedAt) },
      { label: t('system.health.about.uptime'), value: formatDuration(i.uptimeSec * 1000) },
      { label: t('system.health.about.instance'), value: i.instanceId, mono: true },
    ]
  })

  const paths = $derived.by((): KeyValueItem[] => {
    const i = info.data
    if (!i) return []
    return [
      { label: t('system.health.paths.data'), value: i.dataDir, mono: true },
      { label: t('system.health.paths.cache'), value: i.cacheDir, mono: true },
      { label: t('system.health.paths.mounts'), value: i.mountRoot, mono: true },
    ]
  })

  const memory = $derived.by((): KeyValueItem[] => {
    const m = info.data?.memory
    if (!m || !info.data) return []
    return [
      { label: t('system.health.memory.heap'), value: formatBytes(m.allocBytes) },
      { label: t('system.health.memory.sys'), value: formatBytes(m.sysBytes) },
      {
        label: t('system.health.memory.limit'),
        value: m.limitBytes > 0 ? formatBytes(m.limitBytes) : t('system.health.memory.noLimit'),
      },
      { label: t('system.health.memory.gc'), value: formatNumber(m.numGC) },
      { label: t('system.health.memory.goroutines'), value: formatNumber(info.data.goroutines) },
    ]
  })

  const keyKind = $derived(info.data ? masterKeyKind(info.data.masterKeySource) : undefined)
</script>

<div class="page">
  <ChecksPanel
    health={health.data}
    error={health.error}
    loading={health.loading}
    onrefresh={() => health.refresh()}
  />

  <WarningsPanel />

  <div class="cols-2">
    <HostPanel host={host.data} error={host.error} thresholds={thresholds.saved} />
    <div class="stack">
      <DatabasesPanel info={databases.data} error={databases.error} />
      <ThresholdsPanel form={thresholds} />
    </div>
  </div>

  {#if info.error && !info.data}
    <Notice tone="fail" title={t('system.health.about.loadError')}>{errorText(info.error)}</Notice>
  {/if}

  <div class="cols-2">
    <Panel title={t('system.health.about.title')}>
      {#if info.data}
        <KeyValue items={about} />
      {:else}
        <Skeleton height="200px" />
      {/if}
      {#snippet footer()}
        <p class="small muted grow">{t('system.health.about.restartHint')}</p>
        <RestartButton size="sm" />
      {/snippet}
    </Panel>

    <div class="stack">
      <Panel title={t('system.health.paths.title')} description={t('system.health.paths.description')}>
        {#if info.data}
          <KeyValue items={paths}>
            <dt>{t('system.health.paths.key')}</dt>
            <dd>
              {#if keyKind}<span>{t(`system.health.keySource.${keyKind}`)}</span>{/if}
              <span class="mono block">{info.data.masterKeySource}</span>
            </dd>
          </KeyValue>
        {:else}
          <Skeleton height="120px" />
        {/if}
      </Panel>

      <Panel title={t('system.health.memory.title')}>
        {#if info.data}
          <KeyValue items={memory} />
        {:else}
          <Skeleton height="100px" />
        {/if}
      </Panel>
    </div>
  </div>

  <ListenersPanel listeners={info.data?.listeners} />

  {#if session.canOperate}
    <SupportBundle />
  {/if}

  <Panel title={t('system.health.docs.title')} description={t('system.health.docs.description')}>
    <ul class="docs">
      {#each DOCS as d (d.url)}
        <li>
          <a href={d.url} target="_blank" rel="noopener noreferrer">
            {t(d.label)}<Icon name="external" size={16} /><span class="visually-hidden">
              ({t('system.health.docs.newTab')})</span
            >
          </a>
        </li>
      {/each}
    </ul>
  </Panel>
</div>

<style>
  .cols-2 {
    align-items: start;
  }
  .page :global(section[id]) {
    scroll-margin-top: var(--sp-4);
  }
  .grow {
    flex: 1 1 240px;
  }
  .block {
    display: block;
  }
  .docs {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2) var(--sp-5);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .docs a {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
  }
</style>
