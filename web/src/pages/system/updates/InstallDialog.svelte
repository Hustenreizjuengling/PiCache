<!--
  @component
  Confirms installing an update with the current password (like restore and
  API tokens) and queues it (POST /system/update/apply). A wrong password or
  too many attempts stay in the dialog; other refusals (409: not possible in
  this installation, already running, not the available version) close it
  and go to `onerror`. Mount it only while open ({#if}), so the password is
  dropped when it closes.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Dialog, Field, Input, Notice } from '$lib/ui'

  interface Props {
    open?: boolean
    /** Version to install (the available version of the last check). */
    version: string
    /** Version running now. */
    current: string
    onqueued: () => void
    onerror: (err: ApiError) => void
  }

  let { open = $bindable(true), version, current, onqueued, onerror }: Props = $props()

  let password = $state('')
  let submitted = $state(false)
  let busy = $state(false)
  let apiErr = $state.raw<ApiError | null>(null)

  const passwordError = $derived(
    fieldError(apiErr, 'currentPassword') ??
      (submitted && !password ? t('system.updates.install.passwordRequired') : undefined),
  )
  const general = $derived(apiErr && !apiErr.field ? errorText(apiErr) : '')

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    apiErr = null
    if (!password) return
    busy = true
    try {
      await api.system.applyUpdate({ version, currentPassword: password })
      password = ''
      open = false
      onqueued()
    } catch (err) {
      const ae = toApiError(err)
      if (ae.field || ae.code === 'too_many_requests' || ae.code === 'network') {
        apiErr = ae // stays in the dialog: try again
      } else {
        password = ''
        open = false
        onerror(ae)
      }
    } finally {
      busy = false
    }
  }

  const formId = $props.id()
</script>

<Dialog bind:open title={t('system.updates.install.title', { version })} size="sm" dismissible={!busy}>
  <form id="update-{formId}" class="stack" onsubmit={submit} novalidate>
    <p class="small muted">{t('system.updates.install.text', { version, current })}</p>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}
    <!-- Lets password managers offer the right account's password. -->
    <input
      class="visually-hidden"
      type="text"
      name="username"
      autocomplete="username"
      value={session.user?.username ?? ''}
      readonly
      tabindex="-1"
      aria-hidden="true"
    />
    <Field label={t('system.updates.install.password')} error={passwordError} help={t('system.updates.install.passwordHelp')}>
      <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={busy} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="update-{formId}" variant="primary" icon="update" loading={busy}>
      {t('system.updates.install.confirm')}
    </Button>
  {/snippet}
</Dialog>
