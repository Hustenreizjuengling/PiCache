<!--
  @component
  A value to type into a router (an address or network) with a copy
  button; without a value, the text that explains what to enter instead.
-->
<script lang="ts">
  import { CopyButton } from '$lib/ui'

  let { value, missing }: { value?: string; missing?: string } = $props()
</script>

{#if value}
  <span class="cv"><code class="mono">{value}</code><CopyButton text={value} /></span>
{:else}
  <em>{missing}</em>
{/if}

<style>
  .cv {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    max-width: 100%;
    vertical-align: middle;
  }
  code {
    padding: 1px 6px;
    border: 1px solid var(--line);
    border-radius: 4px;
    background: var(--surface-2);
    color: var(--text);
    overflow-wrap: anywhere;
  }
</style>
