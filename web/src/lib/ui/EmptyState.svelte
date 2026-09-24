<!--
  @component
  Tells the user what to do next when there is nothing to show.
  <EmptyState icon="download" title="No downloads yet"
    text="Point a client's DNS at PiCache and start a Steam download.">
    <Button href="#/cache/settings">Open cache settings</Button>
  </EmptyState>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { IconName } from '../icons'
  import Icon from './Icon.svelte'

  interface Props {
    title: string
    text?: string
    icon?: IconName
    /** Less padding (inside tables and small panels). */
    compact?: boolean
    /** Actions (buttons, links). */
    children?: Snippet
  }

  let { title, text, icon, compact = false, children }: Props = $props()
</script>

<div class={['empty', compact && 'compact', compact && !text && !icon && !children && 'plain']}>
  {#if icon}<span class="ic"><Icon name={icon} size={compact ? 20 : 24} /></span>{/if}
  <p class="title">{title}</p>
  {#if text}<p class="text">{text}</p>{/if}
  {#if children}<div class="actions">{@render children()}</div>{/if}
</div>

<style>
  .empty {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-6) var(--sp-5);
    max-width: 60ch;
  }
  .compact {
    padding: var(--sp-4);
  }
  .ic {
    display: flex;
    color: var(--text-3);
  }
  .title {
    font-weight: 600;
  }
  /* A one-line note inside a table: quieter than a full empty state. */
  .plain .title {
    font-weight: 400;
    color: var(--text-2);
  }
  .text {
    color: var(--text-2);
    font-size: var(--fs-sm);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
    margin-top: var(--sp-2);
  }
</style>
