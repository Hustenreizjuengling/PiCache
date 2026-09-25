<!--
  @component
  The outcome of a test message: delivered (with the receiver's HTTP status
  and the time it took), not delivered (the error, the status and a hint
  what to check) or the request could not be made at all. Server texts are
  shown as text.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { isApiError } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDuration } from '$lib/format'
  import { Notice, Spinner } from '$lib/ui'
  import type { TestState } from './channels'

  interface Props {
    /** Channel name for the title. */
    name: string
    test: TestState
    ondismiss?: () => void
  }

  let { name, test, ondismiss }: Props = $props()

  function sentence(s: string): string {
    const x = s.trim()
    return x ? x[0].toUpperCase() + x.slice(1) : x
  }

  const hint = $derived.by(() => {
    const r = test.result
    if (!r || r.ok) return undefined
    if (r.status === 401 || r.status === 403) return t('system.notifications.result.hintAuth')
    if (r.status === 404) return t('system.notifications.result.hintNotFound')
    if (!r.status) return t('system.notifications.result.hintNetwork')
    return undefined
  })
</script>

{#if test.running}
  <p class="sending small muted" role="status">
    <Spinner size={16} />
    <span>{t('system.notifications.result.sending', { name })}</span>
  </p>
{:else if test.result?.ok}
  <Notice tone="ok" title={t('system.notifications.result.ok', { name })} {ondismiss}>
    {test.result.status
      ? t('system.notifications.result.okStatus', { status: String(test.result.status), duration: formatDuration(test.result.durationMs) })
      : t('system.notifications.result.okNoStatus', { duration: formatDuration(test.result.durationMs) })}
  </Notice>
{:else if test.result}
  <Notice tone="fail" title={t('system.notifications.result.failed', { name })} {ondismiss}>
    {#if test.result.error}<p class="err">{sentence(test.result.error)}</p>{/if}
    <p>
      {test.result.status
        ? t('system.notifications.result.failedStatus', { status: String(test.result.status), duration: formatDuration(test.result.durationMs) })
        : t('system.notifications.result.failedNoStatus', { duration: formatDuration(test.result.durationMs) })}
      {#if hint}{' '}{hint}{/if}
    </p>
  </Notice>
{:else if isApiError(test.error, 'too_many_requests')}
  <Notice tone="warn" title={t('system.notifications.result.busyTitle')} {ondismiss}>
    {t('system.notifications.result.busy')}
  </Notice>
{:else if test.error}
  <Notice tone="fail" title={t('system.notifications.result.error')} {ondismiss}>
    {errorText(test.error)}
  </Notice>
{/if}

<style>
  .sending {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-height: 24px;
  }
  .err {
    color: var(--text);
    overflow-wrap: anywhere;
  }
</style>
