<!--
  @component
  Toast host; rendered once in App.svelte. Use the `toast` store to show messages.
  The host is a manual popover (top layer), raised above an open modal dialog
  or side panel whenever a toast arrives, so confirmations stay visible there.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import Icon from './Icon.svelte'
  import IconButton from './IconButton.svelte'
  import { dismiss, hold, release, toasts } from './toast.svelte'

  const icon = { success: 'success', error: 'error', info: 'info' } as const

  let host: HTMLDivElement
  let shown = 0

  $effect(() => {
    host.showPopover()
    return () => {
      if (host.matches(':popover-open')) host.hidePopover()
    }
  })

  // A modal opened after the host covers it: raise the host for new toasts.
  $effect(() => {
    const n = toasts().length
    if (n > shown && document.querySelector('dialog:modal') && host.matches(':popover-open')) {
      host.hidePopover()
      host.showPopover()
    }
    shown = n
  })
</script>

<div bind:this={host} class="host" popover="manual" aria-live="polite" aria-relevant="additions">
  {#each toasts() as item (item.id)}
    <div
      class={['toast', item.kind]}
      role={item.kind === 'error' ? 'alert' : 'status'}
      onmouseenter={() => hold(item.id)}
      onmouseleave={() => release(item.id)}
      onfocusin={() => hold(item.id)}
      onfocusout={() => release(item.id)}
    >
      <span class="ic"><Icon name={icon[item.kind]} /></span>
      <p>{item.message}</p>
      <IconButton icon="close" size="sm" label={t('common.action.dismiss')} onclick={() => dismiss(item.id)} />
    </div>
  {/each}
</div>

<style>
  .host {
    position: fixed;
    inset: auto;
    margin: 0;
    padding: 0;
    border: 0;
    background: transparent;
    overflow: visible;
    right: var(--sp-4);
    bottom: var(--sp-4);
    z-index: 100;
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    width: min(400px, calc(100vw - 2 * var(--sp-4)));
    pointer-events: none;
  }
  .toast {
    --c: var(--focus);
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-2) var(--sp-3) var(--sp-3);
    border: 1px solid var(--line);
    border-left: 4px solid var(--c);
    border-radius: var(--r-control);
    background: var(--surface);
    box-shadow: var(--shadow-float);
    pointer-events: auto;
    animation: toast-in var(--dur-fast) ease-out;
  }
  .success {
    --c: var(--ok);
  }
  .error {
    --c: var(--fail);
  }
  .ic {
    display: flex;
    color: var(--c);
    margin-top: 5px;
  }
  p {
    flex: 1;
    min-width: 0;
    padding-top: 4px;
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  @keyframes toast-in {
    from {
      opacity: 0;
      transform: translateY(8px);
    }
  }
</style>
