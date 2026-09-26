<!--
  @component
  The weekly plan of one group: seven rows (Monday to Sunday) of 24 hours
  with a bar for every window of an enabled schedule. Solid orange = all
  internet blocked, striped orange = selected services blocked (the pair
  colour of "blocked"); overnight windows continue on the next row, Sunday
  into Monday. A marker shows the current time on today's row. Pure CSS; the
  same windows are listed as text for screen readers.
-->
<script lang="ts">
  import { i18n, t } from '$i18n/index.svelte'
  import type { ParentalSchedule, ParentalService } from '$lib/api'
  import { formatClock } from '../../system/backup/schedule'
  import { toHost } from '$lib/hostclock.svelte'
  import { blockText, clockOf, dayName, planSegments, rowOf, WEEK, type Segment } from './plan'

  interface Props {
    schedules: readonly ParentalSchedule[]
    catalog: readonly ParentalService[] | undefined
    /** The current time (ticks while the page is open). */
    now: Date
    /** Accessible name, e.g. "Weekly plan of Kids". */
    label: string
    /** Shown faded (the group is disabled or its restrictions are lifted). */
    faded?: boolean
  }

  let { schedules, catalog, now, label, faded = false }: Props = $props()

  const auto = $props.id()
  const segments = $derived(planSegments(schedules))
  // The marker follows the host's clock, like the schedules.
  const hostNow = $derived(toHost(now))
  const todayRow = $derived(rowOf(hostNow.getDay()))
  const nowPct = $derived(((hostNow.getHours() * 60 + hostNow.getMinutes()) / 1440) * 100)

  function pct(minutes: number): number {
    return (minutes / 1440) * 100
  }

  // A 24-hour clock gets plain numbers (0 6 12 18 24: "0 Uhr" would not fit on
  // phones); a 12-hour clock the language's own labels (12 AM, 6 AM, …).
  const ticks = $derived.by(() => {
    const f = new Intl.DateTimeFormat(i18n.tag, { hour: 'numeric', timeZone: 'UTC' })
    const h12 = /^h1[12]$/.test(f.resolvedOptions().hourCycle ?? '')
    const hours = h12 ? [0, 6, 12, 18] : [0, 6, 12, 18, 24]
    return hours.map((h) => ({ h, label: h12 ? f.format(new Date(Date.UTC(2000, 0, 1, h))) : String(h) }))
  })

  function range(s: Segment): string {
    return `${formatClock(clockOf(s.from))}–${formatClock(clockOf(s.to))}`
  }

  function describe(s: Segment): string {
    const params = { range: range(s), name: s.schedule.name, what: blockText(s.schedule, catalog) }
    return s.kind === 'all' ? t('dns.parental.plan.windowAll', params) : t('dns.parental.plan.windowServices', params)
  }

  /** Text alternative: the windows of each day in time order. */
  const days = $derived(
    WEEK.map((day, row) => ({
      day,
      windows: segments
        .filter((s) => s.row === row)
        .sort((a, b) => a.from - b.from)
        .map(describe),
    })),
  )
</script>

<figure class={['plan', faded && 'faded']} aria-labelledby="plan-{auto}">
  <figcaption id="plan-{auto}" class="visually-hidden">{label}</figcaption>
  <div class="chart" aria-hidden="true">
    <div class="axis">
      {#each ticks as tick (tick.h)}
        <span class={['tick', tick.h === 0 && 'first', tick.h === 24 && 'last']} style:left="{pct(tick.h * 60)}%">{tick.label}</span>
      {/each}
    </div>
    {#each WEEK as day, row (day)}
      <div class={['day', row === todayRow && 'today']}>
        <span class="name">{dayName(day)}</span>
        <div class="track">
          {#each segments.filter((s) => s.row === row) as s, i (i)}
            <span
              class={['bar', s.kind, s.fromPrev && 'from-prev', s.intoNext && 'into-next']}
              style:left="{pct(s.from)}%"
              style:width="{pct(s.to - s.from)}%"
              title="{s.schedule.name}: {range(s)} · {blockText(s.schedule, catalog)}"
            ></span>
          {/each}
          {#if row === todayRow}<span class="now" style:left="{nowPct}%"></span>{/if}
        </div>
      </div>
    {/each}
  </div>
  <ul class="visually-hidden">
    {#each days as d (d.day)}
      <li>
        {d.windows.length > 0
          ? t('dns.parental.plan.dayWindows', { day: dayName(d.day, 'long'), windows: d.windows.join('; ') })
          : t('dns.parental.plan.dayFree', { day: dayName(d.day, 'long') })}
      </li>
    {/each}
  </ul>
  <div class="legend" aria-hidden="true">
    <span><span class="swatch all"></span>{t('dns.parental.plan.all')}</span>
    <span><span class="swatch services"></span>{t('dns.parental.plan.services')}</span>
    <span><span class="swatch now-mark"></span>{t('dns.parental.plan.now')}</span>
  </div>
</figure>

<style>
  .plan {
    --blocked-soft: color-mix(in srgb, var(--orange) 26%, var(--surface));
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    margin: 0;
  }
  .faded .chart {
    opacity: 0.45;
    filter: saturate(0.4);
  }
  .chart {
    display: flex;
    flex-direction: column;
    gap: 5px;
    min-width: 0;
  }
  .axis {
    position: relative;
    height: 16px;
    margin-left: calc(3.25rem + var(--sp-2));
    color: var(--text-3);
    font-size: var(--fs-xs);
    line-height: 16px;
  }
  .tick {
    position: absolute;
    top: 0;
    transform: translateX(-50%);
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }
  .tick.first {
    transform: none;
  }
  .tick.last {
    transform: translateX(-100%);
  }
  .day {
    display: grid;
    grid-template-columns: 3.25rem minmax(0, 1fr);
    align-items: center;
    gap: var(--sp-2);
  }
  .name {
    overflow: hidden;
    color: var(--text-2);
    font-size: var(--fs-xs);
    white-space: nowrap;
    text-overflow: ellipsis;
  }
  .today .name {
    color: var(--text);
    font-weight: 700;
  }
  /* 24 hours: faint hour lines, stronger every 6 hours. */
  .track {
    position: relative;
    height: 18px;
    border-radius: 4px;
    background-color: var(--surface-2);
    background-image:
      linear-gradient(to right, var(--line-strong) 1px, transparent 1px),
      linear-gradient(to right, var(--line) 1px, transparent 1px);
    background-size:
      25% 100%,
      calc(100% / 24) 100%;
  }
  .today .track {
    box-shadow: inset 0 0 0 1px var(--line-strong);
  }
  .bar {
    position: absolute;
    top: 0;
    bottom: 0;
    min-width: 2px;
    border-radius: 4px;
  }
  .bar.from-prev {
    border-top-left-radius: 0;
    border-bottom-left-radius: 0;
  }
  .bar.into-next {
    border-top-right-radius: 0;
    border-bottom-right-radius: 0;
  }
  .bar.all,
  .swatch.all {
    background: var(--orange);
  }
  .bar.services,
  .swatch.services {
    background: repeating-linear-gradient(-45deg, var(--orange) 0 3px, var(--blocked-soft) 3px 6px);
    box-shadow: inset 0 0 0 1px var(--orange);
  }
  /* Now: a line through today's row, kept visible on top of the bars. */
  .now {
    position: absolute;
    top: -4px;
    bottom: -4px;
    width: 2px;
    margin-left: -1px;
    border-radius: 1px;
    background: var(--text);
    box-shadow: 0 0 0 1px var(--surface);
    z-index: 1;
  }
  .now::before {
    content: '';
    position: absolute;
    top: -3px;
    left: -2px;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--text);
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-4);
    margin-left: calc(3.25rem + var(--sp-2));
    color: var(--text-2);
    font-size: var(--fs-xs);
  }
  .legend > span {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .swatch {
    display: inline-block;
    width: 14px;
    height: 10px;
    border-radius: 3px;
  }
  .swatch.now-mark {
    width: 2px;
    height: 12px;
    border-radius: 1px;
    background: var(--text);
  }
  @media (max-width: 480px) {
    .day {
      grid-template-columns: 2.5rem minmax(0, 1fr);
    }
    .axis,
    .legend {
      margin-left: calc(2.5rem + var(--sp-2));
    }
  }
</style>
