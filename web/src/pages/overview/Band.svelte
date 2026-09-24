<!--
  @component
  A broad overview band (one per product half): title row, main content and
  an optional two-column split below a divider.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    title: string
    actions?: Snippet
    children: Snippet
    split?: Snippet
  }

  let { title, actions, children, split }: Props = $props()

  const auto = $props.id()
</script>

<section class="band" aria-labelledby="band-{auto}">
  <header>
    <h2 id="band-{auto}">{title}</h2>
    {#if actions}<div class="actions">{@render actions()}</div>{/if}
  </header>
  <div class="main">{@render children()}</div>
  {#if split}<div class="split">{@render split()}</div>{/if}
</section>

<style>
  .band {
    min-width: 0;
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
  }
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2);
    padding: var(--sp-4) var(--sp-4) var(--sp-2);
  }
  h2 {
    font-size: var(--fs-lg);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
  .main {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    padding-bottom: var(--sp-4);
    min-width: 0;
  }
  .split {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    border-top: 1px solid var(--line);
  }
  .split > :global(*) {
    padding-top: var(--sp-4);
    min-width: 0;
  }
  .split > :global(* + *) {
    border-left: 1px solid var(--line);
  }
  @media (max-width: 1000px) {
    .split {
      grid-template-columns: minmax(0, 1fr);
    }
    .split > :global(* + *) {
      border-left: 0;
      border-top: 1px solid var(--line);
    }
  }
</style>
