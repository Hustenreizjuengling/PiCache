<!--
  @component
  Renders dialogs requested with confirm(); placed once in App.svelte.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import ConfirmDialog from './ConfirmDialog.svelte'
  import { pendingConfirm, settleConfirm } from './confirm.svelte'

  const req = $derived(pendingConfirm())
  let open = $state(false)
  let ok = false

  // A new request opens the dialog.
  $effect(() => {
    if (req) {
      ok = false
      open = true
    }
  })

  // Closing (confirmed, cancelled, Esc) settles the request.
  $effect(() => {
    if (!open && untrack(() => req)) settleConfirm(ok)
  })
</script>

{#if req}
  <ConfirmDialog
    bind:open
    title={req.title}
    message={req.message}
    confirmLabel={req.confirmLabel}
    cancelLabel={req.cancelLabel}
    danger={req.danger ?? true}
    onconfirm={async () => {
      await req.action?.()
      ok = true
    }}
  />
{/if}
