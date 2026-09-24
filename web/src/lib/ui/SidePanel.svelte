<!--
  @component
  Drawer from the right for row details and actions (table row click opens
  it; no page change). Same behaviour as Dialog (Esc, focus trap).
  <SidePanel bind:open title={row.qname} subtitle={row.clientIp}>
    <KeyValue items={…} />
    {#snippet actions()}<Button onclick={block}>Block domain</Button>{/snippet}
  </SidePanel>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import Dialog from './Dialog.svelte'

  interface Props {
    open?: boolean
    title: string
    subtitle?: string
    /** md = 560 px, lg = 760 px. */
    size?: 'md' | 'lg'
    dismissible?: boolean
    onclose?: () => void
    actions?: Snippet
    children: Snippet
  }

  let { open = $bindable(false), size = 'md', ...rest }: Props = $props()
</script>

<Dialog bind:open drawer {size} {...rest} />
