<!--
  @component
  Toast host. The page-level host is rendered once in App.svelte; every open
  Dialog/SidePanel renders its own (`dialog`), because a modal dialog makes
  everything outside it inert (a toast there could not be closed or held).
  Only the innermost open host shows the toasts (toast.svelte.ts). Hosts are
  manual popovers (top layer). A dialog's host keeps to the bottom-right
  corner unless that would cover the dialog; then it sits just above the
  dialog's footer, so the action buttons stay visible and usable.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import Icon from './Icon.svelte'
  import IconButton from './IconButton.svelte'
  import { activeHost, dismiss, hold, registerHost, release, toasts } from './toast.svelte'

  interface Props {
    /** Rendered inside a modal Dialog (used by Dialog only). */
    dialog?: boolean
  }

  let { dialog = false }: Props = $props()

  const icon = { success: 'success', error: 'error', info: 'info' } as const
  const GAP = 16 // var(--sp-4)

  let host: HTMLDivElement
  let id = $state(-1) // not showing anything until mounted

  $effect(() => {
    if (!dialog) {
      id = 0
      return
    }
    const reg = registerHost()
    id = reg.id
    return reg.unregister
  })

  const active = $derived(activeHost() === id)
  const items = $derived(active ? toasts() : [])

  // Shown for the host's lifetime. A dialog's host is shown after its dialog
  // opened, so it is above the dialog in the top layer.
  $effect(() => {
    const show = () => {
      if (host.isConnected && !host.matches(':popover-open')) host.showPopover()
    }
    const dlg = dialog ? host.closest('dialog') : null
    if (dlg && !dlg.open) queueMicrotask(show)
    else show()
    return () => {
      if (host.matches(':popover-open')) host.hidePopover()
    }
  })

  // Toasts held by pointer or focus can leave this host without mouseleave or
  // focusout (another host took over, or the dialog closed): restart them.
  const held = new Set<number>()
  function holdItem(n: number) {
    held.add(n)
    hold(n)
  }
  function releaseItem(n: number) {
    if (held.delete(n)) release(n)
  }
  $effect(() => {
    void active
    return () => {
      for (const n of held) release(n)
      held.clear()
    }
  })

  function place() {
    const dlg = host.closest('dialog')
    host.style.right = host.style.bottom = host.style.width = ''
    if (!dlg || items.length === 0) return
    const d = dlg.getBoundingClientRect()
    const h = host.getBoundingClientRect()
    const vw = document.documentElement.clientWidth
    const vh = window.innerHeight
    const covers = vw - GAP - h.width < d.right && vw - GAP > d.left && vh - GAP - h.height < d.bottom && vh - GAP > d.top
    if (!covers) return
    const footer = dlg.querySelector(':scope > .frame > footer')
    const edge = footer ? footer.getBoundingClientRect().top : d.bottom
    host.style.bottom = `${Math.max(GAP, vh - edge + GAP / 2)}px`
    host.style.right = `${Math.max(GAP, vw - d.right + GAP)}px`
    host.style.width = `${Math.max(0, Math.min(400, d.width - 2 * GAP))}px`
  }

  $effect(() => {
    if (!dialog) return
    void items.length
    place()
    window.addEventListener('resize', place)
    return () => window.removeEventListener('resize', place)
  })
</script>

<div bind:this={host} class="host" popover="manual" aria-live="polite" aria-relevant="additions">
  {#each items as item (item.id)}
    <div
      class={['toast', item.kind]}
      role={item.kind === 'error' ? 'alert' : 'status'}
      onmouseenter={() => holdItem(item.id)}
      onmouseleave={() => releaseItem(item.id)}
      onfocusin={() => holdItem(item.id)}
      onfocusout={() => releaseItem(item.id)}
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
