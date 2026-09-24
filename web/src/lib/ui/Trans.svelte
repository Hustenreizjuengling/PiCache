<!--
  @component
  Renders a translation whose placeholders are components (links, chips),
  keeping word order per language without HTML in dictionaries.
  Dictionary: 'overview.sentence.dns': 'DNS is answering {rate}'
  <Trans key="overview.sentence.dns">
    {#snippet rate()}<Stat value="42" label="queries/min" href="#/dns/queries" />{/snippet}
  </Trans>
  Snippets are matched by placeholder name; `params` fill plain-text placeholders.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { tParts, type MessageKey, type Params } from '../../i18n/index.svelte'

  interface Props {
    key: MessageKey
    params?: Params
    [slot: string]: unknown
  }

  let { key, params, ...slots }: Props = $props()

  const parts = $derived(tParts(key, params))
</script>

{#each parts as part, i (i)}{#if part.slot !== undefined}{#if typeof slots[part.slot] === 'function'}{@render (slots[part.slot] as Snippet)()}{:else}{`{${part.slot}}`}{/if}{:else}{part.text}{/if}{/each}
