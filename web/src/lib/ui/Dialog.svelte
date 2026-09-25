<!--
  @component
  Modal dialog on the native <dialog> element (focus trap, Esc, inert
  background). Content is rendered only while open, so forms start fresh.
  Toasts raised while it is open are shown in (and above) the dialog.
  <Dialog bind:open title="Add blocklist">
    <form id="add-list" onsubmit={…}>…</form>
    {#snippet actions()}
      <Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
      <Button variant="primary" type="submit" form="add-list">Add blocklist</Button>
    {/snippet}
  </Dialog>
  `drawer` is used by SidePanel.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import IconButton from './IconButton.svelte'
  import Toasts from './Toasts.svelte'

  interface Props {
    open?: boolean
    title: string
    /** Secondary line under the title (e.g. the object's id). */
    subtitle?: string
    size?: 'sm' | 'md' | 'lg'
    /** Esc, backdrop click and the close button close the dialog (disable while saving). */
    dismissible?: boolean
    /** Drawer from the right (SidePanel). */
    drawer?: boolean
    onclose?: () => void
    actions?: Snippet
    children: Snippet
  }

  let {
    open = $bindable(false),
    title,
    subtitle,
    size = 'md',
    dismissible = true,
    drawer = false,
    onclose,
    actions,
    children,
  }: Props = $props()

  let dlg: HTMLDialogElement
  let downOnBackdrop = false
  const auto = $props.id()
  const titleId = `dlg-${auto}`

  $effect(() => {
    if (open && !dlg.open) {
      dlg.showModal()
      // Focus the dialog itself rather than its first control (the close
      // button): screen readers announce the title, Tab reaches the controls,
      // and a panel opened from a link or the URL shows no focus ring on a
      // button nobody chose.
      dlg.focus()
    } else if (!open && dlg.open) dlg.close()
  })

  function handleClose() {
    if (open) open = false
    onclose?.()
  }

  function handleCancel(e: Event) {
    if (!dismissible) e.preventDefault()
  }

  function requestClose() {
    if (dismissible) dlg.close()
  }
</script>

<!-- Backdrop clicks close the dialog; keyboard users have Esc and the close button. -->
<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dlg}
  tabindex="-1"
  class={['dlg', size, drawer && 'drawer']}
  aria-labelledby={titleId}
  onclose={handleClose}
  oncancel={handleCancel}
  onmousedown={(e) => (downOnBackdrop = e.target === dlg)}
  onclick={(e) => {
    if (downOnBackdrop && e.target === dlg) requestClose()
    downOnBackdrop = false
  }}
>
  {#if open}
    <div class="frame">
      <header>
        <div class="titles">
          <h2 id={titleId}>{title}</h2>
          {#if subtitle}<p class="subtitle">{subtitle}</p>{/if}
        </div>
        <IconButton icon="close" label={t('common.action.close')} disabled={!dismissible} onclick={requestClose} />
      </header>
      <div class="body">{@render children()}</div>
      {#if actions}<footer>{@render actions()}</footer>{/if}
      <!-- The page-level toasts are inert behind a modal dialog: show them here. -->
      <Toasts dialog />
    </div>
  {/if}
</dialog>

<style>
  .dlg {
    width: min(560px, calc(100vw - 2 * var(--sp-4)));
    max-height: calc(100vh - 2 * var(--sp-6));
    padding: 0;
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-float);
    overflow: hidden;
  }
  .dlg[open] {
    display: flex;
    animation: dlg-in var(--dur-fast) ease-out;
  }
  /* The dialog is focused on open (see above); its controls keep their rings. */
  .dlg:focus {
    outline: none;
  }
  .sm {
    width: min(420px, calc(100vw - 2 * var(--sp-4)));
  }
  .lg {
    width: min(820px, calc(100vw - 2 * var(--sp-4)));
  }
  .dlg::backdrop {
    background: rgb(10 14 20 / 0.45);
  }
  .drawer {
    width: min(560px, 100vw);
    height: 100vh;
    max-height: 100vh;
    margin: 0 0 0 auto;
    border-radius: 0;
    border-width: 0 0 0 1px;
  }
  .drawer.lg {
    width: min(760px, 100vw);
  }
  .drawer[open] {
    animation-name: drawer-in;
  }
  .frame {
    display: flex;
    flex-direction: column;
    width: 100%;
    min-height: 0;
    max-height: inherit;
  }
  header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-3);
    padding: var(--sp-4) var(--sp-3) var(--sp-3) var(--sp-5);
    border-bottom: 1px solid var(--line);
  }
  .titles {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    min-width: 0;
    padding-top: 6px;
  }
  h2 {
    font-size: var(--fs-lg);
    overflow-wrap: anywhere;
  }
  .subtitle {
    font-size: var(--fs-sm);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .body {
    flex: 1;
    min-height: 0;
    overflow: auto;
    padding: var(--sp-5);
  }
  footer {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-5);
    border-top: 1px solid var(--line);
    background: var(--surface);
  }
  @keyframes dlg-in {
    from {
      opacity: 0;
      transform: translateY(6px) scale(0.99);
    }
  }
  @keyframes drawer-in {
    from {
      transform: translateX(24px);
      opacity: 0;
    }
  }
  @media (max-width: 480px) {
    .body {
      padding: var(--sp-4);
    }
    header {
      padding-left: var(--sp-4);
    }
    footer {
      padding: var(--sp-3) var(--sp-4);
    }
  }
</style>
