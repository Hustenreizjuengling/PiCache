<!--
  @component
  Release notes (Markdown from GitHub) rendered as Svelte elements: text
  only, never HTML from the notes; links only for https URLs, opened in a new
  tab. Long notes are cut after about `limit` characters with "Show more".
  <ReleaseNotes source={latest.notes} />
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { Button, Icon } from '$lib/ui'
  import { limitBlocks, parseMarkdown, type Block, type Inline } from './markdown'

  interface Props {
    source: string
    /** Characters of text shown before "Show more". */
    limit?: number
    /** Accessible name of the notes region. */
    label?: string
  }

  let { source, limit = 1200, label }: Props = $props()

  const blocks = $derived(parseMarkdown(source))
  let expanded = $state(false)
  const shown = $derived(expanded ? { blocks, truncated: false } : limitBlocks(blocks, limit))
  const canCollapse = $derived(expanded && limitBlocks(blocks, limit).truncated)

  // The panel title is an h2: the notes' own headings start at h3, keeping
  // their relative levels ("## 1.3.0" → h3, "### Added" → h4).
  const base = $derived(Math.min(6, ...blocks.filter((b) => b.type === 'heading').map((b) => b.level)))

  function tag(level: number): string {
    return `h${Math.min(6, 3 + level - base)}`
  }

  const id = $props.id()
</script>

{#snippet inline(nodes: Inline[])}{#each nodes as n, i (i)}{#if n.type === 'text'}{n.text}{:else if n.type === 'code'}<code class={['mono', n.text.length <= 32 && 'short']}>{n.text}</code>{:else if n.type === 'strong'}<strong>{@render inline(n.children)}</strong>{:else if n.type === 'em'}<em>{@render inline(n.children)}</em>{:else}<a href={n.href} target="_blank" rel="noopener noreferrer">{@render inline(n.children)}<Icon name="external" size={14} /><span class="visually-hidden">{` (${t('system.updates.notes.newTab')})`}</span></a>{/if}{/each}{/snippet}

{#snippet render(list: Block[])}
  {#each list as b, i (i)}
    {#if b.type === 'heading'}
      <svelte:element this={tag(b.level)} class="h">{@render inline(b.content)}</svelte:element>
    {:else if b.type === 'paragraph'}
      <p>{@render inline(b.content)}</p>
    {:else if b.type === 'list'}
      <svelte:element this={b.ordered ? 'ol' : 'ul'} start={b.ordered && b.start !== 1 ? b.start : undefined}>
        {#each b.items as item, j (j)}
          <li>
            {#if item.length === 1 && item[0].type === 'paragraph'}
              {@render inline(item[0].content)}
            {:else}
              {@render render(item)}
            {/if}
          </li>
        {/each}
      </svelte:element>
    {:else if b.type === 'code'}
      <pre class="mono">{b.text}</pre>
    {:else if b.type === 'quote'}
      <blockquote>{@render render(b.blocks)}</blockquote>
    {:else}
      <hr />
    {/if}
  {/each}
{/snippet}

<div class="notes" id="notes-{id}" role="region" aria-label={label}>
  {@render render(shown.blocks)}
</div>
{#if shown.truncated || canCollapse}
  <div class="more">
    <Button
      variant="ghost"
      size="sm"
      icon={expanded ? 'chevron-up' : 'chevron-down'}
      aria-expanded={expanded}
      aria-controls="notes-{id}"
      onclick={() => (expanded = !expanded)}
    >
      {expanded ? t('system.updates.notes.less') : t('system.updates.notes.more')}
    </Button>
  </div>
{/if}

<style>
  .notes {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    font-size: var(--fs-sm);
    line-height: var(--lh);
    overflow-wrap: anywhere;
  }
  .notes :global(.h) {
    margin-top: var(--sp-2);
    font-size: var(--fs-md);
    font-weight: 600;
    line-height: var(--lh-tight);
  }
  .notes :global(.h:first-child) {
    margin-top: 0;
  }
  .notes :global(ul),
  .notes :global(ol) {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding-left: var(--sp-5);
  }
  .notes :global(li > ul),
  .notes :global(li > ol),
  .notes :global(li > p + p) {
    margin-top: var(--sp-1);
  }
  .notes :global(code) {
    padding: 0 3px;
    border-radius: 4px;
    background: var(--surface-3);
    font-size: 0.95em;
  }
  /* Short commands and flags stay on one line ("--check"). */
  .notes :global(code.short) {
    white-space: nowrap;
  }
  .notes :global(pre) {
    margin: 0;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    overflow-x: auto;
    white-space: pre;
    overflow-wrap: normal;
  }
  .notes :global(blockquote) {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding-left: var(--sp-3);
    border-left: 3px solid var(--line-strong);
    color: var(--text-2);
  }
  .notes :global(hr) {
    width: 100%;
    margin: var(--sp-1) 0;
    border: 0;
    border-top: 1px solid var(--line);
  }
  .notes :global(a) {
    display: inline;
  }
  .notes :global(a .icon) {
    margin-left: 2px;
    vertical-align: -2px;
  }
  .more {
    margin-top: calc(-1 * var(--sp-2));
    margin-left: calc(-1 * var(--sp-2));
  }
</style>
