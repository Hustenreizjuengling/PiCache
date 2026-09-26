<!--
  @component
  Application log (admins): the service's recent log records (an in-memory
  ring; the full log stays in the system journal) with a minimum level and a
  component filter, followed live over SSE (pause/resume), and a download
  of the loaded records as NDJSON. The newest 500 records load first;
  "Show older" loads every record the ring keeps (2000). Debug logging can
  be turned on for a while. Secrets are redacted by the server; client addresses and domains
  are masked while the privacy settings say so.
  Query: ?level=debug|info|warn|error&component=<name>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, streamSystemLog, type LiveStream, type LogRecord } from '$lib/api'
  import { fileStamp, saveBlob } from '$lib/download'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatTime, sameDay } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, IconButton, Notice, Panel, Select, Skeleton } from '$lib/ui'
  import LevelPanel from './applog/LevelPanel.svelte'
  import { asLevel, LEVEL_TONES, LEVELS, matches, toNdjson } from './applog/records'

  /** Records loaded at first; "Show older" loads up to MAX (all the ring keeps), the stream adds to them up to MAX. */
  const LIMIT = 500
  const MAX = 2000

  const level = $derived(asLevel(router.param('level')))
  const component = $derived(router.param('component').trim())
  let limit = $state(LIMIT)

  const log = resource((signal) =>
    session.canOperate
      ? api.system.log({ level, component: component || undefined, limit }, { signal })
      : Promise.resolve(undefined),
  )
  /** The server may hold older records of these filters than the page loaded. */
  const hasOlder = $derived(limit < MAX && (log.data?.records.length ?? 0) >= limit)

  let live = $state.raw<LogRecord[]>([])
  let stream = $state<LiveStream<LogRecord> | null>(null)

  $effect(() => {
    if (!session.canOperate) return
    const lv = level
    const comp = component
    const s = streamSystemLog(
      { level: lv, component: comp || undefined },
      {
        onEvents: (batch) => {
          const add = batch.filter((r) => matches(r, lv, comp)).reverse()
          if (add.length > 0) live = [...add, ...live].slice(0, MAX)
        },
        // Records may have been missed: start again from a fresh page.
        onReconnect: async () => {
          await log.refresh()
          live = []
        },
      },
    )
    untrack(() => {
      live = []
      stream = s
    })
    return () => {
      s.close()
      stream = null
    }
  })

  /** Newest first, each sequence number once (the stream may repeat what the page holds). */
  const records = $derived.by(() => {
    const seen = new Set<number>()
    const out: LogRecord[] = []
    for (const r of [...live, ...(log.data?.records ?? [])]) {
      if (seen.has(r.seq)) continue
      seen.add(r.seq)
      out.push(r)
    }
    return out.sort((a, b) => b.seq - a.seq).slice(0, MAX)
  })

  const levelOptions = $derived(LEVELS.map((l) => ({ value: l, label: t(`system.applog.levelMin.${l}`) })))
  const componentOptions = $derived([
    { value: '', label: t('system.applog.allComponents') },
    ...[...new Set([...(log.data?.components ?? []), ...(component ? [component] : [])])].map((c) => ({ value: c, label: c })),
  ])

  const streamLabel = $derived.by(() => {
    switch (stream?.state) {
      case 'open':
        return t('system.applog.live.open')
      case 'paused':
        return t('system.applog.live.paused')
      case 'retrying':
        return t('system.applog.live.retrying')
      default:
        return t('system.applog.live.connecting')
    }
  })
  const streamTone = $derived(stream?.state === 'open' ? 'ok' : stream?.state === 'retrying' ? 'warn' : 'neutral')

  const today = $derived(new Date().toDateString())

  function timeText(iso: string): string {
    return sameDay(iso, today) ? formatTime(iso, true) : formatDateTime(iso, true)
  }

  function download() {
    saveBlob(toNdjson(records), `picache-log-${fileStamp()}.ndjson`)
  }
</script>

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('system.applog.adminOnly')}</Notice>
  {:else}
    {#if log.data}
      <LevelPanel
        baseLevel={log.data.baseLevel}
        override={log.data.override}
        components={log.data.components}
        onchange={(st) => log.data && log.set({ ...log.data, baseLevel: st.baseLevel, override: st.override })}
      />
    {/if}

    <Panel
      title={t('system.applog.title')}
      description={log.data ? t('system.applog.description', { capacity: formatNumber(log.data.capacity) }) : undefined}
      flush
    >
      {#snippet actions()}
        <Button size="sm" icon="download" disabled={records.length === 0} onclick={download}>{t('system.applog.download')}</Button>
      {/snippet}

      <div class="toolbar pad">
        <div class="f">
          <Select
            size="sm"
            aria-label={t('system.applog.level')}
            value={level}
            options={levelOptions}
            onchange={(e) => router.setQuery({ level: e.currentTarget.value === 'debug' ? null : e.currentTarget.value })}
          />
        </div>
        <div class="f">
          <Select
            size="sm"
            aria-label={t('system.applog.component')}
            value={component}
            options={componentOptions}
            onchange={(e) => router.setQuery({ component: e.currentTarget.value })}
          />
        </div>
        {#if stream}
          <span class="live">
            <Chip size="sm" tone={streamTone} label={streamLabel} />
            {#if stream.paused}
              <IconButton icon="play" variant="secondary" size="sm" label={t('system.applog.live.resume')} onclick={() => stream?.resume()} />
            {:else}
              <IconButton icon="pause" variant="secondary" size="sm" label={t('system.applog.live.pause')} onclick={() => stream?.pause()} />
            {/if}
          </span>
        {/if}
        <span class="spacer"></span>
        <span class="small muted" aria-live="polite">{t('system.applog.count', { count: formatNumber(records.length) })}</span>
      </div>

      {#if log.data && log.data.dropped > 0}
        <p class="dropped small">{t('system.applog.dropped', { count: formatNumber(log.data.dropped) })}</p>
      {/if}

      {#if log.error && !log.data}
        <div class="pad-b">
          <Notice tone="fail">
            {errorText(log.error)}
            {#snippet actions()}
              <Button size="sm" icon="refresh" onclick={() => log.refresh()}>{t('common.action.retry')}</Button>
            {/snippet}
          </Notice>
        </div>
      {:else if !log.data}
        <div class="pad-b"><Skeleton height="240px" /></div>
      {:else if records.length === 0}
        <EmptyState compact icon="terminal" title={t('system.applog.empty')} text={t('system.applog.emptyText')} />
      {:else}
        <ol class="records" aria-label={t('system.applog.title')}>
          {#each records as r (r.seq)}
            <li class={['rec', r.level.toLowerCase()]}>
              <span class="time mono" title={formatDateTime(r.time, true)}>{timeText(r.time)}</span>
              <span class="lvl"><Chip size="sm" tone={LEVEL_TONES[r.level] ?? 'neutral'} label={r.level} /></span>
              <span class="comp mono">{r.component}</span>
              <span class="body">
                <span class="msg">{r.msg}</span>
                {#if r.attrs.length > 0}
                  <span class="attrs mono">
                    {#each r.attrs as a, i (i)}
                      <span class="attr"><span class="k">{a.key}</span>=<span class="v">{a.value}</span></span>
                    {/each}
                  </span>
                {/if}
              </span>
            </li>
          {/each}
        </ol>
        {#if hasOlder}
          <div class="pad-t">
            <Button size="sm" icon="chevron-down" loading={log.loading} onclick={() => (limit = MAX)}>{t('system.applog.older')}</Button>
          </div>
        {:else if records.length >= MAX}
          <p class="small muted pad-b">{t('system.applog.capped', { count: formatNumber(MAX) })}</p>
        {/if}
      {/if}
    </Panel>

    <p class="small muted">{t('system.applog.journal')}</p>
  {/if}
</div>

<style>
  .pad {
    padding: 0 var(--sp-4) var(--sp-3);
    align-items: center;
  }
  .pad-b {
    padding: 0 var(--sp-4) var(--sp-4);
  }
  .pad-t {
    padding: var(--sp-3) var(--sp-4);
    border-top: 1px solid var(--line);
  }
  .f {
    flex: 0 1 200px;
    min-width: 140px;
  }
  .live {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .dropped {
    padding: 0 var(--sp-4) var(--sp-3);
    color: var(--warn);
  }
  .records {
    margin: 0;
    padding: 0;
    list-style: none;
    border-top: 1px solid var(--line);
    font-size: var(--fs-sm);
    max-height: max(420px, calc(100vh - 360px));
    overflow: auto;
    overscroll-behavior: contain;
  }
  .rec {
    display: grid;
    grid-template-columns: max-content 64px 11ch minmax(0, 1fr);
    align-items: baseline;
    gap: 2px var(--sp-3);
    padding: 6px var(--sp-4);
    border-bottom: 1px solid var(--line);
  }
  .rec:last-child {
    border-bottom: 0;
  }
  .rec.warn {
    background: color-mix(in srgb, var(--warn) 6%, var(--surface));
  }
  .rec.error {
    background: color-mix(in srgb, var(--fail) 7%, var(--surface));
  }
  .time {
    color: var(--text-2);
    white-space: nowrap;
  }
  .comp {
    color: var(--text-2);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .msg {
    overflow-wrap: anywhere;
  }
  .attrs {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-3);
    color: var(--text-2);
    font-size: var(--fs-xs);
  }
  .attr {
    overflow-wrap: anywhere;
  }
  .k {
    color: var(--text-3);
  }
  @media (max-width: 640px) {
    .rec {
      grid-template-columns: max-content max-content minmax(0, 1fr);
    }
    .body {
      grid-column: 1 / -1;
    }
  }
</style>
