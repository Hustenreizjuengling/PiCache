<!--
  @component
  Host resources from the last health evaluation (GET /system/host, sampled
  every minute): device model, CPUs, load, uptime, memory and swap, the
  memory limit of the container or of the PiCache service (its cgroup),
  temperatures and the data and cache disks.
  Values that could not be read are left out. In a container load, uptime
  and memory are the host's and labelled so. Values beyond the warning
  thresholds are marked.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, HealthSettings, HostInfo } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDuration, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { Badge, KeyValue, Meter, Notice, Panel, Skeleton, type KeyValueItem, type Tone } from '$lib/ui'

  interface Props {
    host?: HostInfo
    error?: ApiError
    /** Saved thresholds (marks values beyond them). */
    thresholds?: HealthSettings
  }

  let { host, error, thresholds }: Props = $props()

  const c = $derived(!!host?.container)

  function load(n: number): string {
    return formatNumber(n, 2)
  }

  const items = $derived.by((): KeyValueItem[] => {
    const h = host
    if (!h) return []
    const out: KeyValueItem[] = []
    if (h.model) out.push({ label: t('system.health.host.model'), value: h.model })
    out.push({ label: t('system.health.host.cpus'), value: formatNumber(h.cpus) })
    if (h.uptimeSec !== undefined) {
      out.push({
        label: c ? t('system.health.host.uptimeHost') : t('system.health.host.uptime'),
        value: formatDuration(h.uptimeSec * 1000),
      })
    }
    return out
  })

  /** 15-minute load per CPU above the threshold. */
  const loadHigh = $derived(!!host?.load && !!thresholds && host.load.fifteen > thresholds.loadPerCpuMax * Math.max(1, host.cpus))

  interface Bar {
    key: string
    label: string
    text: string
    used: number
    max: number
    tone: Tone
  }

  function lowMemory(available: number, total: number): boolean {
    return !!thresholds && total > 0 && (available / total) * 100 < thresholds.memoryAvailableMinPercent
  }

  const bars = $derived.by((): Bar[] => {
    const h = host
    if (!h) return []
    const out: Bar[] = []
    const m = h.memory
    if (m && m.totalBytes > 0) {
      out.push({
        key: 'memory',
        label: c ? t('system.health.host.memoryHost') : t('system.health.host.memory'),
        text: t('system.health.host.memoryText', {
          used: formatBytes(m.usedBytes),
          available: formatBytes(m.availableBytes),
          total: formatBytes(m.totalBytes),
        }),
        used: m.usedBytes,
        max: m.totalBytes,
        tone: lowMemory(m.availableBytes, m.totalBytes) ? 'warn' : 'info',
      })
      if (m.swapTotalBytes > 0) {
        out.push({
          key: 'swap',
          label: t('system.health.host.swap'),
          text: t('system.health.host.usedOf', { used: formatBytes(m.swapUsedBytes), total: formatBytes(m.swapTotalBytes) }),
          used: m.swapUsedBytes,
          max: m.swapTotalBytes,
          tone: 'neutral',
        })
      }
    }
    const g = h.cgroup
    if (g && g.limitBytes > 0) {
      out.push({
        key: 'cgroup',
        // Outside a container the cgroup limit is the service's (e.g. MemoryMax= of the unit).
        label: c ? t('system.health.host.cgroup') : t('system.health.host.cgroupService'),
        text: t('system.health.host.usedOf', { used: formatBytes(g.usageBytes), total: formatBytes(g.limitBytes) }),
        used: g.usageBytes,
        max: g.limitBytes,
        tone: lowMemory(g.availableBytes, g.limitBytes) ? 'warn' : 'info',
      })
    }
    for (const d of h.disks) {
      if (d.totalBytes <= 0) continue
      out.push({
        key: `disk-${d.role}-${d.path}`,
        label: d.role === 'cache' ? t('system.health.host.diskCache', { path: d.path }) : t('system.health.host.diskData', { path: d.path }),
        text: t('system.health.host.freeOf', {
          free: formatBytes(d.freeBytes),
          total: formatBytes(d.totalBytes),
          percent: formatPercent(d.freeBytes / d.totalBytes),
        }),
        used: d.totalBytes - d.freeBytes,
        max: d.totalBytes,
        tone: 'neutral',
      })
    }
    return out
  })

  const empty = $derived(!!host && !host.load && !host.memory && !host.cgroup && host.uptimeSec === undefined && host.temperatures.length === 0 && host.disks.length === 0)
</script>

<Panel
  id="host"
  title={t('system.health.host.title')}
  description={host ? t('system.health.host.description', { time: formatRelative(host.sampledAt) }) : undefined}
>
  {#if error && !host}
    <Notice tone="fail" title={t('system.health.host.loadError')}>{errorText(error)}</Notice>
  {:else if !host}
    <Skeleton height="220px" />
  {:else}
    <div class="stack">
      {#if c}<p class="small muted">{t('system.health.host.container')}</p>{/if}
      <KeyValue {items}>
        {#if host.load}
          <dt>{c ? t('system.health.host.loadHost') : t('system.health.host.load')}</dt>
          <dd>
            <span class="num-row">{load(host.load.one)} · {load(host.load.five)} · {load(host.load.fifteen)}</span>
            {#if loadHigh}<Badge tone="warn">{t('system.health.host.loadHigh')}</Badge>{/if}
          </dd>
        {/if}
      </KeyValue>

      {#each bars as b (b.key)}
        <div class="bar">
          <div class="bar-head">
            <span class="lbl">{b.label}</span>
            <span class="val">{b.text}</span>
          </div>
          <Meter label={b.label} max={b.max} segments={[{ label: b.label, value: b.used, text: b.text, tone: b.tone }]} legend={false} />
        </div>
      {/each}

      {#if host.temperatures.length > 0}
        <section class="stack-sm" aria-labelledby="host-temps">
          <h3 id="host-temps">{t('system.health.host.temperatures')}</h3>
          <ul class="temps">
            {#each host.temperatures as z (z.zone)}
              {@const hot = !!thresholds && z.celsius >= thresholds.temperatureMaxCelsius}
              <li>
                <span class="zone"><span class="mono">{z.type || z.zone}</span>{#if z.type}<span class="subtle small">{z.zone}</span>{/if}</span>
                <span class={['deg', hot && 'hot']}>{t('system.health.host.celsius', { value: formatNumber(z.celsius, 1) })}</span>
                {#if hot}<Badge tone="warn">{t('system.health.host.tooHot')}</Badge>{/if}
              </li>
            {/each}
          </ul>
        </section>
      {/if}

      {#if empty}<p class="small muted">{t('system.health.host.noData')}</p>{/if}
    </div>
  {/if}
</Panel>

<style>
  .bar {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
  }
  .bar-head {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    justify-content: space-between;
    gap: 0 var(--sp-3);
    font-size: var(--fs-sm);
  }
  .lbl {
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .val,
  .num-row {
    font-variant-numeric: tabular-nums;
  }
  .num-row {
    margin-right: var(--sp-2);
  }
  h3 {
    font-size: var(--fs-sm);
    font-weight: 600;
    color: var(--text-2);
  }
  .temps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
  }
  .temps li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
  }
  .zone {
    display: inline-flex;
    align-items: baseline;
    gap: var(--sp-2);
    min-width: 12em;
  }
  .deg {
    font-variant-numeric: tabular-nums;
    font-weight: 600;
  }
  .deg.hot {
    color: var(--warn);
  }
</style>
