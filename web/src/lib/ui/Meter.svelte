<!--
  @component
  Horizontal stacked bar with a legend, e.g. storage used/free.
  <Meter label="Cache storage" max={total} segments={[
    { label: 'Cached', value: cached, text: formatBytes(cached), pair: 'green' },
    { label: 'Other files', value: other, text: formatBytes(other), tone: 'neutral' },
  ]} rest={{ label: 'Free', text: formatBytes(free) }} />
-->
<script lang="ts">
  import type { MeterSegment } from './types'

  interface Props {
    /** Accessible name of the whole meter. */
    label: string
    max: number
    segments: MeterSegment[]
    /** Legend entry for the unfilled remainder (e.g. free space). */
    rest?: { label: string; text?: string }
    /** Marker at a position from the left (0..max), e.g. where the minimum free space begins. */
    marker?: { value: number; label: string }
    legend?: boolean
  }

  let { label, max, segments, rest, marker, legend = true }: Props = $props()

  function colorOf(s: MeterSegment): string {
    if (s.pair) return `var(--${s.pair})`
    switch (s.tone) {
      case 'ok':
        return 'var(--ok)'
      case 'warn':
        return 'var(--warn)'
      case 'fail':
        return 'var(--fail)'
      case 'info':
        return 'var(--focus)'
      default:
        return 'var(--text-3)'
    }
  }

  const pct = (v: number) => (max > 0 ? Math.max(0, Math.min(100, (v / max) * 100)) : 0)
  const summary = $derived(
    [label, ...segments.map((s) => `${s.label}: ${s.text ?? s.value}`), rest ? `${rest.label}: ${rest.text ?? ''}` : '']
      .filter(Boolean)
      .join(', '),
  )
</script>

<div class="meter">
  <div class="bar" role="img" aria-label={summary}>
    {#each segments as s (s.label)}
      <span class="seg" style:width="{pct(s.value)}%" style:background={colorOf(s)}></span>
    {/each}
    {#if marker && max > 0}
      <span class="marker" style:left="{pct(marker.value)}%" title={marker.label}></span>
    {/if}
  </div>
  {#if legend}
    <ul class="legend" aria-hidden="true">
      {#each segments as s (s.label)}
        <li><span class="sw" style:background={colorOf(s)}></span>{s.label}{#if s.text}<span class="val">{s.text}</span>{/if}</li>
      {/each}
      {#if rest}
        <li><span class="sw free"></span>{rest.label}{#if rest.text}<span class="val">{rest.text}</span>{/if}</li>
      {/if}
    </ul>
  {/if}
</div>

<style>
  .meter {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .bar {
    position: relative;
    display: flex;
    height: 12px;
    overflow: hidden;
    border-radius: var(--r-pill);
    background: var(--surface-3);
  }
  .seg {
    height: 100%;
  }
  .marker {
    position: absolute;
    top: 0;
    bottom: 0;
    width: 2px;
    margin-left: -1px;
    background: var(--text);
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-4);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .legend li {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .sw {
    width: 10px;
    height: 10px;
    border-radius: 3px;
  }
  .sw.free {
    background: var(--surface-3);
    box-shadow: inset 0 0 0 1px var(--line-strong);
  }
  .val {
    color: var(--text);
    font-weight: 600;
  }
</style>
