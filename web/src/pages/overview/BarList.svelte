<!--
  @component
  A compact ranked bar list of the DNS band (blocked by purpose, query
  types): top to bottom, then into the next column. Bars are orange
  (blocked), striped blue (safe search: answered, but restricted) or
  neutral; an item with `href` links to the matching query-log view.
-->
<script lang="ts" module>
  export interface BarItem {
    key: string
    label: string
    count: number
    fill?: 'orange' | 'safe' | 'neutral'
    href?: string
    /** Machine value (a record type): monospace. */
    mono?: boolean
  }
</script>

<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { formatNumber } from '../../lib/format'
  import { Button, Skeleton } from '../../lib/ui'

  interface Props {
    title: string
    /** Shown after the title, e.g. the real start of the counted window ("since 09:00"). */
    note?: string
    /** Explanation of the note (tooltip). */
    noteTitle?: string
    items?: BarItem[]
    loading?: boolean
    error?: string
    onretry?: () => void
    emptyText: string
  }

  let { title, note, noteTitle, items, loading = false, error, onretry, emptyText }: Props = $props()

  const auto = $props.id()
  const shown = $derived((items ?? []).filter((p) => p.count > 0))
  const max = $derived(Math.max(1, ...shown.map((p) => p.count)))
</script>

<section class="bars-panel" aria-labelledby="bars-{auto}">
  <h3 id="bars-{auto}">
    {title}{#if note}<span class="note" title={noteTitle}>· {note}</span>{/if}
  </h3>
  {#if error && !items}
    <p class="err">
      {error}
      {#if onretry}<Button size="sm" variant="ghost" icon="refresh" onclick={onretry}>{t('common.action.retry')}</Button>{/if}
    </p>
  {:else if !items}
    {#if loading}<Skeleton height="72px" />{/if}
  {:else if shown.length === 0}
    <p class="small muted">{emptyText}</p>
  {:else}
    <ol class="bars">
      {#each shown as p (p.key)}
        <li>
          {#if p.href}
            <a class={['name', p.mono && 'mono']} href={p.href}>{p.label}</a>
          {:else}
            <span class={['name', p.mono && 'mono']}>{p.label}</span>
          {/if}
          <span class="bar" aria-hidden="true">
            <span class={['fill', p.fill ?? 'orange']} style:width="{(p.count / max) * 100}%"></span>
          </span>
          <span class="n">{formatNumber(p.count)}</span>
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  .bars-panel {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: 0 var(--sp-4);
    min-width: 0;
  }
  h3 {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .note {
    margin-left: var(--sp-2);
    color: var(--text-3);
    font-weight: 400;
  }
  /* Ranked top to bottom, then into the next column. */
  .bars {
    columns: 3 260px;
    column-gap: var(--sp-6);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(40px, 72px) 6ch;
    align-items: center;
    gap: var(--sp-2);
    padding: 3px 0;
    break-inside: avoid;
    font-size: var(--fs-sm);
    line-height: 1.3;
  }
  .name {
    overflow-wrap: anywhere;
  }
  a.name {
    color: var(--text);
    text-decoration: none;
  }
  a.name:hover {
    text-decoration: underline;
  }
  .bar {
    display: flex;
    height: 6px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
    overflow: hidden;
  }
  .fill {
    height: 100%;
    border-radius: var(--r-pill);
  }
  .fill.orange {
    background: var(--orange);
  }
  .fill.neutral {
    background: var(--text-3);
  }
  /* Answered, but restricted: the striped blue of a secondary "answered" state. */
  .fill.safe {
    background: repeating-linear-gradient(-45deg, var(--blue) 0 3px, color-mix(in srgb, var(--blue) 30%, var(--surface)) 3px 6px);
    box-shadow: inset 0 0 0 1px var(--blue);
  }
  .n {
    font-variant-numeric: tabular-nums;
    text-align: right;
  }
  .err {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
