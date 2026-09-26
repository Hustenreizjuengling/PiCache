<!--
  @component
  The running version and what the last update check found: up to date,
  update available, not checked yet or why the check failed. Admins can
  check now. Explains development builds (compared by their base version)
  and pre-releases.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { UpdateInfo } from '$lib/api'
  import { formatDate, formatDateTime, formatRelative } from '$lib/format'
  import type { IconName } from '$lib/icons'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Icon, Notice, Panel, type Tone } from '$lib/ui'
  import { safeHref } from './markdown'
  import { baseVersion, isPrerelease } from './version'

  interface Props {
    info: UpdateInfo
    /** An update is being installed (no checks meanwhile). */
    busy: boolean
    checking: boolean
    oncheck: () => void
  }

  let { info, busy, checking, oncheck }: Props = $props()

  const cur = $derived(info.current)
  const base = $derived(info.currentIsDevBuild ? baseVersion(cur.version) : undefined)
  const prerelease = $derived(!info.currentIsDevBuild && isPrerelease(cur.version))
  const releaseUrl = $derived(info.latest ? safeHref(info.latest.url) : undefined)

  interface Summary {
    tone: Tone
    icon: IconName
    title: string
    text?: string
    hint?: string
  }

  const summary = $derived.by((): Summary => {
    const latest = info.latest
    if (info.status?.state === 'running') {
      return { tone: 'info', icon: 'update', title: t('system.updates.summary.installing', { version: info.status.version }) }
    }
    if (info.checkError) {
      const msg = info.checkError.trim()
      return {
        tone: 'warn',
        icon: 'alert',
        title: t('system.updates.summary.error'),
        text: msg.charAt(0).toUpperCase() + msg.slice(1),
        hint: info.checkEnabled ? t('system.updates.summary.errorRetry') : undefined,
      }
    }
    if (info.updateAvailable && latest) {
      return {
        tone: 'info',
        icon: 'update',
        title: t('system.updates.summary.available', { version: latest.version }),
        text: t('system.updates.summary.published', { date: formatDate(latest.publishedAt) }),
      }
    }
    if (!info.checkedAt) {
      return {
        tone: 'neutral',
        icon: 'clock',
        title: t('system.updates.summary.never'),
        text: info.checkEnabled
          ? t('system.updates.summary.neverAuto')
          : session.isAdmin
            ? t('system.updates.summary.neverOff')
            : t('system.updates.summary.neverOffRead'),
      }
    }
    return {
      tone: 'ok',
      icon: 'success',
      title: t('system.updates.summary.upToDate'),
      text: !latest
        ? t('system.updates.summary.noRelease')
        : latest.version === cur.version
          ? t('system.updates.summary.newest')
          : t('system.updates.summary.latestIs', { version: latest.version }),
    }
  })

  const modeLabel = $derived(t(`system.updates.mode.${info.mode}`))

  const lastRun = $derived(info.status && info.status.state !== 'running' ? info.status : undefined)
</script>

<Panel title={t('system.updates.current.title')}>
  {#snippet actions()}
    {#if info.checkedAt}
      <span class="small muted" title={formatDateTime(info.checkedAt, true)}>
        {t('system.updates.checkedAt', { time: formatRelative(info.checkedAt) })}
      </span>
    {/if}
    {#if session.canOperate}
      <Button size="sm" icon="refresh" loading={checking} disabled={busy} onclick={oncheck}>
        {t('system.updates.check')}
      </Button>
    {/if}
  {/snippet}

  <div class="stack">
    <div class={['summary', summary.tone]} role="status">
      <span class="ic"><Icon name={summary.icon} size={22} /></span>
      <div class="stack-sm grow">
        <p class="title">{summary.title}</p>
        {#if summary.text}
          <p class="small muted text">{summary.text}</p>
        {/if}
        {#if summary.hint}<p class="small muted">{summary.hint}</p>{/if}
        {#if info.updateAvailable && info.latest && releaseUrl && !info.checkError}
          <p class="small">
            <a href={releaseUrl} target="_blank" rel="noopener noreferrer">
              {t('system.updates.release.github')}<Icon name="external" size={14} /><span class="visually-hidden"
                >{` (${t('system.updates.notes.newTab')})`}</span
              >
            </a>
          </p>
        {/if}
      </div>
    </div>

    <dl class="kv">
      <dt>{t('system.updates.current.version')}</dt>
      <dd class="badges">
        <span class="mono">{cur.version}</span>
        {#if info.currentIsDevBuild}<Badge tone="warn">{t('system.updates.current.devBuild')}</Badge>{/if}
        {#if prerelease}<Badge tone="info">{t('system.updates.current.prerelease')}</Badge>{/if}
      </dd>
      <dt>{t('system.updates.current.commit')}</dt>
      <dd class="mono">{cur.commit || '–'}</dd>
      <dt>{t('system.updates.current.built')}</dt>
      <dd>{Number.isNaN(Date.parse(cur.date)) ? cur.date || '–' : formatDateTime(cur.date)}</dd>
      <dt>{t('system.updates.current.install')}</dt>
      <dd>{modeLabel}</dd>
      <dt>{t('system.updates.current.auto')}</dt>
      <dd>
        {info.checkEnabled
          ? info.includePrereleases
            ? t('system.updates.current.autoOnPre')
            : t('system.updates.current.autoOn')
          : t('system.updates.current.autoOff')}
      </dd>
      {#if lastRun}
        <dt>{t('system.updates.current.lastRun')}</dt>
        <dd>
          <span class="mono">{lastRun.from}</span> → <span class="mono">{lastRun.version}</span>:
          {t(`system.updates.runState.${lastRun.state}`)}
          {#if lastRun.finishedAt}
            <span class="muted" title={formatDateTime(lastRun.finishedAt, true)}>
              ({formatRelative(lastRun.finishedAt)})</span
            >
          {/if}
        </dd>
      {/if}
    </dl>

    {#if info.currentIsDevBuild}
      <Notice tone="info" title={t('system.updates.dev.title')}>
        {base ? t('system.updates.dev.base', { base }) : t('system.updates.dev.noBase')}
      </Notice>
    {:else if prerelease && !info.includePrereleases}
      <Notice tone="info" title={t('system.updates.pre.title')}>{t('system.updates.pre.text')}</Notice>
    {/if}
  </div>
</Panel>

<style>
  .summary {
    --c: var(--text-3);
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .summary.ok {
    --c: var(--ok);
  }
  .summary.info {
    --c: var(--focus);
  }
  .summary.warn {
    --c: var(--warn);
  }
  .ic {
    display: flex;
    color: var(--c);
  }
  .title {
    font-size: var(--fs-lg);
    font-weight: 600;
    line-height: var(--lh-tight);
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .text {
    overflow-wrap: anywhere;
  }
  a {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
  }
  .kv {
    display: grid;
    grid-template-columns: minmax(120px, max-content) minmax(0, 1fr);
    gap: var(--sp-2) var(--sp-4);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    color: var(--text-2);
  }
  dd {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  dd.badges {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  @media (max-width: 480px) {
    .kv {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    dd {
      margin-bottom: var(--sp-2);
    }
  }
</style>
