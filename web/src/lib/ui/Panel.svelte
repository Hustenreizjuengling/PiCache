<!--
  @component
  Bordered surface with an optional title row and actions. Tables sit inside
  panels with `flush` (no body padding).
  <Panel title="Blocklists" description="…">
    {#snippet actions()}<Button icon="plus">Add blocklist</Button>{/snippet}
    <Table … />
  </Panel>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    title?: string
    description?: string
    /** Heading level of the title (default 2). */
    level?: 2 | 3
    /** No padding around the body (tables, lists). */
    flush?: boolean
    id?: string
    actions?: Snippet
    footer?: Snippet
    children: Snippet
  }

  let { title, description, level = 2, flush = false, id, actions, footer, children }: Props = $props()

  const auto = $props.id()
  const headingId = `panel-${auto}`
</script>

<section class="panel" {id} aria-labelledby={title ? headingId : undefined}>
  {#if title || actions}
    <header>
      <div class="titles">
        {#if title}<svelte:element this={`h${level}`} id={headingId} class="title">{title}</svelte:element>{/if}
        {#if description}<p class="desc">{description}</p>{/if}
      </div>
      {#if actions}<div class="actions">{@render actions()}</div>{/if}
    </header>
  {/if}
  <div class={['body', flush && 'flush', !(title || actions) && 'untitled']}>
    {@render children()}
  </div>
  {#if footer}<footer>{@render footer()}</footer>{/if}
</section>

<style>
  .panel {
    min-width: 0;
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
  }
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-4);
    padding: var(--sp-4) var(--sp-4) 0;
  }
  .titles {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    min-width: 0;
  }
  .title {
    font-size: var(--fs-md);
    font-weight: 600;
  }
  .desc {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
    align-items: center;
  }
  .body {
    padding: var(--sp-4);
    min-width: 0;
  }
  .body.flush {
    padding: var(--sp-3) 0 0;
  }
  .body.flush.untitled {
    padding: 0;
  }
  footer {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-4);
    border-top: 1px solid var(--line);
  }
</style>
