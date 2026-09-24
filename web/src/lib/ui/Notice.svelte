<!--
  @component
  Inline message or banner: what happened and how to fix it.
  <Notice tone="warn" title="The NAS share is not mounted">
    PiCache is serving downloads uncached. Mount /srv/picache/nas or check the storage settings.
    {#snippet actions()}<Button size="sm" href="#/cache/storage">Open storage</Button>{/snippet}
  </Notice>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import type { IconName } from '../icons'
  import Icon from './Icon.svelte'
  import IconButton from './IconButton.svelte'

  interface Props {
    tone?: 'info' | 'ok' | 'warn' | 'fail'
    title?: string
    icon?: IconName
    /** Shows a close button. */
    ondismiss?: () => void
    actions?: Snippet
    children?: Snippet
  }

  let { tone = 'info', title, icon, ondismiss, actions, children }: Props = $props()

  const defaultIcon: Record<string, IconName> = { info: 'info', ok: 'success', warn: 'alert', fail: 'error' }
</script>

<div class={['notice', tone]} role={tone === 'fail' ? 'alert' : 'status'}>
  <span class="ic"><Icon name={icon ?? defaultIcon[tone]} /></span>
  <div class="content">
    {#if title}<p class="title">{title}</p>{/if}
    {#if children}<div class="text">{@render children()}</div>{/if}
    {#if actions}<div class="actions">{@render actions()}</div>{/if}
  </div>
  {#if ondismiss}
    <IconButton icon="close" size="sm" label={t('common.action.dismiss')} onclick={ondismiss} />
  {/if}
</div>

<style>
  .notice {
    --c: var(--focus);
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid color-mix(in srgb, var(--c) 40%, var(--surface));
    border-left: 4px solid var(--c);
    border-radius: var(--r-control);
    background: color-mix(in srgb, var(--c) 8%, var(--surface));
    color: var(--text);
    min-width: 0;
  }
  .ok {
    --c: var(--ok);
  }
  .warn {
    --c: var(--warn);
  }
  .fail {
    --c: var(--fail);
  }
  .ic {
    display: flex;
    color: var(--c);
    margin-top: 1px;
  }
  .content {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    flex: 1;
    min-width: 0;
  }
  .title {
    font-weight: 600;
  }
  .text {
    color: var(--text-2);
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  .text :global(ul) {
    margin: var(--sp-1) 0 0;
    padding-left: var(--sp-5);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
    margin-top: var(--sp-2);
  }
</style>
