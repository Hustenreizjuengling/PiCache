<!--
  @component
  Copies text to the clipboard (works on plain-HTTP LAN addresses too).
  <CopyButton text={snippet} /> · <CopyButton text={token} label="Copy token" showLabel />
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import Button from './Button.svelte'
  import IconButton from './IconButton.svelte'
  import { toast } from './toast.svelte'

  interface Props {
    text: string
    label?: string
    /** Button with text instead of an icon-only button. */
    showLabel?: boolean
    size?: 'sm' | 'md'
  }

  let { text, label, showLabel = false, size = 'sm' }: Props = $props()

  let copied = $state(false)
  let timer: ReturnType<typeof setTimeout> | undefined
  let live: HTMLSpanElement

  async function write(value: string): Promise<boolean> {
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(value)
        return true
      } catch {
        /* fall through to the legacy path */
      }
    }
    return legacyCopy(value)
  }

  /**
   * Plain HTTP (not a secure context): hidden textarea + execCommand. While a
   * modal dialog is open everything outside it is inert, so the textarea goes
   * into the dialog holding the button (a textarea in <body> could not take
   * the selection, and execCommand would still report success).
   */
  function legacyCopy(value: string): boolean {
    const container = live?.closest('dialog') ?? document.body
    const prev = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const ta = document.createElement('textarea')
    ta.value = value
    ta.setAttribute('readonly', '')
    ta.setAttribute('aria-hidden', 'true')
    ta.tabIndex = -1
    Object.assign(ta.style, { position: 'fixed', top: '0', left: '0', width: '1px', height: '1px', opacity: '0' })
    container.appendChild(ta)
    let ok = false
    try {
      ta.focus({ preventScroll: true })
      ta.select()
      ta.setSelectionRange(0, ta.value.length) // (line breaks are normalised)
      // Only a selection in the textarea is copied: anything else is a failure.
      const selected = document.activeElement === ta && ta.selectionStart === 0 && ta.selectionEnd === ta.value.length
      ok = selected && document.execCommand('copy')
    } catch {
      ok = false
    }
    ta.remove()
    prev?.focus({ preventScroll: true })
    return ok
  }

  async function copy() {
    if (await write(text)) {
      copied = true
      clearTimeout(timer)
      timer = setTimeout(() => (copied = false), 1500)
    } else {
      toast.error(t('common.copy.failed'))
    }
  }

  $effect(() => () => clearTimeout(timer))

  const name = $derived(copied ? t('common.copy.done') : (label ?? t('common.action.copy')))
</script>

{#if showLabel}
  <Button {size} icon={copied ? 'check' : 'copy'} onclick={copy}>{name}</Button>
{:else}
  <IconButton {size} icon={copied ? 'check' : 'copy'} label={name} onclick={copy} />
{/if}
<span bind:this={live} class="visually-hidden" aria-live="polite">{copied ? t('common.copy.done') : ''}</span>
