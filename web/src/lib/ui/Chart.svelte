<!--
  @component
  Time-series chart (uPlot) themed with the pair colours; follows light/dark
  and resizes with its container. The legend below doubles as the tooltip
  (values at the cursor). Stacked series are drawn as bands; the legend shows
  each series' own value.

  <Chart label="DNS queries per minute" timestamps={s.timestamps} stacked
         series={[{ label: 'Allowed', pair: 'blue', values: allowed }, { label: 'Blocked', pair: 'orange', values: blocked }]}
         yFormat={formatCompact} valueFormat={(v) => formatNumber(v)} />

  timestamps are unix seconds. `dashed` marks a secondary state of the same meaning.
  The y axis starts at 0 and reaches at least `minMax` (an idle chart keeps a
  sensible scale, e.g. 1 MB/s for throughput); ticks are whole numbers unless
  `integer` is false.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import uPlot from 'uplot'
  import 'uplot/dist/uPlot.min.css'
  import { i18n, t } from '../../i18n/index.svelte'
  import { formatAxisTime, formatCompact, formatDateTime } from '../format'
  import { theme } from '../theme.svelte'
  import Skeleton from './Skeleton.svelte'
  import type { ChartSeries } from './types'

  interface Props {
    /** Accessible name of the chart. */
    label: string
    timestamps: number[]
    series: ChartSeries[]
    stacked?: boolean
    height?: number
    /** y axis labels. */
    yFormat?: (v: number) => string
    /** Legend values (default: yFormat). */
    valueFormat?: (v: number) => string
    loading?: boolean
    /** The y axis extends at least to this value (default 1). */
    minMax?: number
    /** Whole-number y ticks: steps of 1, 2, 5 × 10ⁿ (default true). */
    integer?: boolean
  }

  let {
    label,
    timestamps,
    series,
    stacked = false,
    height = 220,
    yFormat = formatCompact,
    valueFormat,
    loading = false,
    minMax = 1,
    integer = true,
  }: Props = $props()

  /** Tick steps 1, 2, 5 × 10ⁿ: no fractional ticks for counts and bytes. */
  const INT_INCRS = Array.from({ length: 16 }, (_, e) => [1, 2, 5].map((m) => m * 10 ** e)).flat()

  let el: HTMLDivElement
  let plot: uPlot | null = null
  // Raw (unstacked) values for the legend.
  let raw: (number | null)[][] = []

  function cssVar(name: string): string {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  }

  function colorOf(s: ChartSeries): string {
    return cssVar(s.pair ? `--${s.pair}` : (s.colorVar ?? '--text-3')) || '#888888'
  }

  function withAlpha(color: string, alpha: number): string {
    if (/^#[0-9a-f]{6}$/i.test(color)) {
      return color + Math.round(alpha * 255).toString(16).padStart(2, '0')
    }
    return color
  }

  function buildData(): uPlot.AlignedData {
    raw = series.map((s) => s.values)
    if (!stacked) return [timestamps, ...raw]
    const acc = new Array<number>(timestamps.length).fill(0)
    const out = raw.map((vals) =>
      timestamps.map((_, i) => {
        acc[i] += vals[i] ?? 0
        return acc[i]
      }),
    )
    return [timestamps, ...out]
  }

  /**
   * Right padding for the x axis: its labels are centred on their ticks, and
   * the last tick can sit at the right edge, so half of the widest label must
   * fit beside the plot (else "Sep 25" is cut to "Sep 2"). Measured over the
   * hours of a day and the months of a year in both label formats.
   */
  function xLabelPad(font: string): number {
    const ctx = document.createElement('canvas').getContext('2d')
    if (!ctx) return 32
    ctx.font = font
    let widest = 0
    for (let i = 0; i < 24; i++) {
      const hour = formatAxisTime(new Date(2026, 8, 28, i, 0).getTime() / 1000, 3600)
      const day = formatAxisTime(new Date(2026, i % 12, 28).getTime() / 1000, 7 * 86400)
      widest = Math.max(widest, ctx.measureText(hour).width, ctx.measureText(day).width)
    }
    return Math.max(8, Math.ceil(widest / 2) + 2)
  }

  function fmtValue(v: number | null | undefined): string {
    if (v === null || v === undefined || !Number.isFinite(v)) return '–'
    return (valueFormat ?? yFormat)(v)
  }

  /** Data index shown in the legend: the hovered point, else the latest one. */
  function shown(u: uPlot, idx: number | null): number | null {
    if (idx !== null) return idx
    const n = u.data[0]?.length ?? 0
    return n > 0 ? n - 1 : null
  }

  function build(width: number): uPlot.Options {
    const font = `12px ${cssVar('--font') || 'system-ui'}`
    const axisColor = cssVar('--text-3')
    const gridColor = cssVar('--line')
    const opts: uPlot.Options = {
      width,
      height,
      padding: [8, xLabelPad(font), 0, 0],
      cursor: { drag: { x: false, y: false }, points: { size: 6 } },
      legend: { live: true },
      scales: {
        x: { time: true },
        y: {
          range: (_u, _min, max) => [0, Math.max(minMax, max > 0 ? (uPlot.rangeNum(0, max, 0.1, true)[1] ?? max) : 0)],
        },
      },
      axes: [
        {
          stroke: axisColor,
          font,
          grid: { show: false },
          ticks: { stroke: gridColor, size: 4 },
          space: 90,
          values: (u, splits) => {
            const span = (u.scales.x.max ?? 0) - (u.scales.x.min ?? 0)
            return splits.map((v) => formatAxisTime(v, span))
          },
        },
        {
          stroke: axisColor,
          font,
          grid: { stroke: gridColor, width: 1 },
          ticks: { show: false },
          incrs: integer ? INT_INCRS : undefined,
          values: (_u, splits) => splits.map((v) => yFormat(v)),
          size: (_u, values) => {
            const longest = Math.max(0, ...(values ?? []).map((v) => String(v).length))
            return Math.max(36, longest * 7 + 14)
          },
        },
      ],
      series: [
        {
          label: t('common.chart.time'),
          value: (u, _v, _si, idx) => {
            const i = shown(u, idx)
            return i === null ? '–' : formatDateTime(u.data[0][i] * 1000)
          },
        },
        ...series.map((s, i) => {
          const c = colorOf(s)
          return {
            label: s.label,
            stroke: c,
            width: 1.5,
            dash: s.dashed ? [6, 4] : undefined,
            fill: s.fill === false ? undefined : withAlpha(c, s.dashed ? 0.16 : 0.32),
            points: { show: false },
            value: (u: uPlot, _v: number | null, _si: number, idx: number | null) => {
              const at = shown(u, idx)
              return fmtValue(at === null ? null : raw[i]?.[at])
            },
          } satisfies uPlot.Series
        }),
      ],
    }
    if (stacked && series.length > 1) {
      opts.bands = series.slice(1).map((_, i) => ({ series: [i + 2, i + 1] as [number, number] }))
    }
    return opts
  }

  const structure = $derived(
    JSON.stringify([stacked, height, minMax, integer, series.map((s) => [s.label, s.pair, s.colorVar, s.dashed, s.fill])]),
  )

  // (Re)create on structure or theme changes.
  $effect(() => {
    void structure
    void theme.effective
    void i18n.locale // axis and legend formats
    const u = untrack(() => new uPlot(build(el.clientWidth || 600), buildData(), el))
    plot = u
    const ro = new ResizeObserver(() => {
      if (el.clientWidth > 0) u.setSize({ width: el.clientWidth, height })
    })
    ro.observe(el)
    return () => {
      ro.disconnect()
      u.destroy()
      if (plot === u) plot = null
    }
  })

  // Data updates without re-creating the chart.
  $effect(() => {
    void timestamps
    void series.map((s) => s.values)
    const u = untrack(() => plot)
    if (u) u.setData(untrack(buildData))
  })

  const empty = $derived(!loading && timestamps.length === 0)
</script>

<figure class="chart" aria-label={label}>
  <div bind:this={el} class={['plot', (empty || (loading && timestamps.length === 0)) && 'hidden']}></div>
  {#if loading && timestamps.length === 0}
    <div class="placeholder" style:height="{height}px"><Skeleton height="100%" /></div>
  {:else if empty}
    <div class="placeholder msg" style:height="{height}px">{t('common.chart.noData')}</div>
  {/if}
</figure>

<style>
  .chart {
    position: relative;
    margin: 0;
    min-width: 0;
  }
  .plot {
    width: 100%;
    min-width: 0;
  }
  .hidden {
    position: absolute;
    visibility: hidden;
    pointer-events: none;
  }
  .placeholder {
    display: flex;
    align-items: center;
    justify-content: center;
  }
  .msg {
    border: 1px dashed var(--line);
    border-radius: var(--r-control);
    color: var(--text-3);
    font-size: var(--fs-sm);
  }
  /* The legend is a read-out, not a toggle (stacks would break). */
  .chart :global(.u-legend .u-series) {
    pointer-events: none;
  }
</style>
