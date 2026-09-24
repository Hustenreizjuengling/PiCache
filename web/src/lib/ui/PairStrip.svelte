<!--
  @component
  The pair strip: a 6 px bar whose four segments are the live share of DNS
  allowed (blue), blocked (orange), cache hit (green) and WAN (brown) traffic.
  DNS and cache each take half (all of it if the other half had no traffic);
  DNS is split by query count, the cache by bytes. Hover or focus shows the legend.
  <PairStrip allowed={q - b} blocked={b} hit={hitBytes} wan={wanBytes} />
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { formatBytes, formatNumber, formatPercent } from '../format'
  import { placeTooltip } from './position'

  interface Props {
    allowed: number
    blocked: number
    hit: number
    wan: number
    /** Context for the legend, e.g. "Last 15 minutes". */
    caption?: string
  }

  let { allowed, blocked, hit, wan, caption }: Props = $props()

  const auto = $props.id()
  const legendId = `strip-${auto}`
  let strip: HTMLDivElement
  let legend: HTMLDivElement

  const shares = $derived.by(() => {
    const a = Math.max(0, allowed || 0)
    const b = Math.max(0, blocked || 0)
    const h = Math.max(0, hit || 0)
    const w = Math.max(0, wan || 0)
    const dns = a + b
    const cache = h + w
    const dnsHalf = dns > 0 ? (cache > 0 ? 0.5 : 1) : 0
    const cacheHalf = cache > 0 ? (dns > 0 ? 0.5 : 1) : 0
    return {
      allowed: dns > 0 ? (a / dns) * dnsHalf : 0,
      blocked: dns > 0 ? (b / dns) * dnsHalf : 0,
      hit: cache > 0 ? (h / cache) * cacheHalf : 0,
      wan: cache > 0 ? (w / cache) * cacheHalf : 0,
      blockedRatio: dns > 0 ? b / dns : 0,
      hitRatio: cache > 0 ? h / cache : 0,
      any: dns + cache > 0,
    }
  })

  const rows = $derived([
    { key: 'allowed', pair: 'blue', label: t('common.strip.allowed'), value: formatNumber(allowed), share: shares.allowed },
    { key: 'blocked', pair: 'orange', label: t('common.strip.blocked'), value: formatNumber(blocked), share: shares.blocked },
    { key: 'hit', pair: 'green', label: t('common.strip.hit'), value: formatBytes(hit), share: shares.hit },
    { key: 'wan', pair: 'brown', label: t('common.strip.wan'), value: formatBytes(wan), share: shares.wan },
  ])

  const summary = $derived(
    shares.any
      ? t('common.strip.summary', {
          queries: formatNumber(allowed + blocked),
          blocked: formatPercent(shares.blockedRatio),
          bytes: formatBytes(hit + wan),
          hit: formatPercent(shares.hitRatio),
        })
      : t('common.strip.none'),
  )

  function show() {
    legend.showPopover()
    placeTooltip(strip, legend)
  }

  function hide() {
    if (legend.matches(':popover-open')) legend.hidePopover()
  }
</script>

<!-- Focusable so keyboard users can open the legend; the label summarises it for screen readers. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
<div
  bind:this={strip}
  class="strip"
  role="img"
  tabindex="0"
  aria-label={(caption ? caption + ': ' : '') + summary}
  onpointerenter={show}
  onpointerleave={hide}
  onfocus={show}
  onblur={hide}
>
  {#each rows as r (r.key)}
    <span class="seg" style:width="{r.share * 100}%" style:background="var(--{r.pair})"></span>
  {/each}
</div>

<div bind:this={legend} id={legendId} popover="manual" class="legend" aria-hidden="true">
  {#if caption}<p class="cap">{caption}</p>{/if}
  <table>
    <tbody>
      {#each rows as r (r.key)}
        <tr>
          <td><span class="sw" style:background="var(--{r.pair})"></span>{r.label}</td>
          <td class="num">{r.value}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  <p class="sum">{summary}</p>
</div>

<style>
  .strip {
    display: flex;
    width: 100%;
    height: 6px;
    background: var(--surface-3);
    overflow: hidden;
    cursor: default;
  }
  .strip:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 0;
    height: 8px;
  }
  .seg {
    height: 100%;
    transition: width var(--dur-strip) ease;
  }
  .legend {
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-float);
    font-size: var(--fs-sm);
    pointer-events: none;
  }
  .cap {
    font-weight: 600;
    margin-bottom: var(--sp-2);
  }
  table {
    border-collapse: collapse;
  }
  td {
    padding: 2px 0;
  }
  td.num {
    padding-left: var(--sp-5);
    font-weight: 600;
  }
  .sw {
    display: inline-block;
    width: 10px;
    height: 10px;
    margin-right: var(--sp-2);
    border-radius: 3px;
    vertical-align: -1px;
  }
  .sum {
    margin-top: var(--sp-2);
    color: var(--text-2);
    max-width: 280px;
  }
</style>
