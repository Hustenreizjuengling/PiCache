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

  async function write(value: string): Promise<boolean> {
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(value)
        return true
      } catch {
        /* fall through to the legacy path */
      }
    }
    // Plain HTTP (not a secure context): hidden textarea + execCommand.
    const ta = document.createElement('textarea')
    ta.value = value
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    let ok = false
    try {
      ok = document.execCommand('copy')
    } catch {
      ok = false
    }
    ta.remove()
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
<span class="visually-hidden" aria-live="polite">{copied ? t('common.copy.done') : ''}</span>
