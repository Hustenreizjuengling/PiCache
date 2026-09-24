<!--
  @component
  Confirmation for destructive actions; the title names the object
  ("Delete list HaGeZi Multi?"). While `onconfirm` runs the dialog shows a
  spinner; if it throws, the error is shown and the dialog stays open.
  <ConfirmDialog bind:open title="Delete list HaGeZi Multi?" message="…"
    confirmLabel="Delete list" onconfirm={() => api.filter.lists.remove(id)} />
  For one-off confirmations prefer the imperative confirm() from lib/ui.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import { errorText } from '../errors'
  import Button from './Button.svelte'
  import Dialog from './Dialog.svelte'
  import Notice from './Notice.svelte'

  interface Props {
    open?: boolean
    title: string
    message?: string
    confirmLabel: string
    cancelLabel?: string
    /** Red confirm button (default true). */
    danger?: boolean
    onconfirm: () => unknown
    oncancel?: () => void
    children?: Snippet
  }

  let {
    open = $bindable(false),
    title,
    message,
    confirmLabel,
    cancelLabel,
    danger = true,
    onconfirm,
    oncancel,
    children,
  }: Props = $props()

  let busy = $state(false)
  let error = $state('')
  let confirmed = false

  $effect(() => {
    if (open) {
      error = ''
      confirmed = false
    }
  })

  async function confirm() {
    busy = true
    error = ''
    try {
      await onconfirm()
      confirmed = true
      open = false
    } catch (err) {
      error = errorText(err)
    } finally {
      busy = false
    }
  }

  function closed() {
    if (!confirmed) oncancel?.()
  }
</script>

<Dialog bind:open {title} size="sm" dismissible={!busy} onclose={closed}>
  <div class="stack-sm">
    {#if message}<p>{message}</p>{/if}
    {@render children?.()}
    {#if error}<Notice tone="fail">{error}</Notice>{/if}
  </div>
  {#snippet actions()}
    <Button variant="ghost" disabled={busy} onclick={() => (open = false)}>
      {cancelLabel ?? t('common.action.cancel')}
    </Button>
    <Button variant={danger ? 'danger' : 'primary'} loading={busy} onclick={confirm}>{confirmLabel}</Button>
  {/snippet}
</Dialog>
