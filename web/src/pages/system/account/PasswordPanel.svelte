<!--
  @component
  Change the password. The server signs out all other sessions afterwards
  and revokes the API tokens unless "Keep API tokens" is ticked (off by
  default: after a compromise a token may be what the intruder kept), so
  `onchanged` lets the page reload the sessions list.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Checkbox, Field, Input, Notice, Panel, toast } from '$lib/ui'
  import { charCount, MIN_PASSWORD } from '../forms'
  import PasswordStrength from './PasswordStrength.svelte'

  let { onchanged }: { onchanged?: () => void } = $props()

  let current = $state('')
  let next = $state('')
  let repeat = $state('')
  let keepTokens = $state(false)
  let submitted = $state(false)
  let busy = $state(false)
  let apiErr = $state.raw<ApiError | null>(null)

  const errors = $derived({
    current:
      fieldError(apiErr, 'currentPassword') ??
      (submitted && !current ? t('system.account.password.currentRequired') : undefined),
    next:
      fieldError(apiErr, 'newPassword') ??
      (submitted && charCount(next) < MIN_PASSWORD
        ? t('system.account.password.tooShort', { min: MIN_PASSWORD })
        : submitted && next === current
          ? t('system.account.password.same')
          : undefined),
    repeat: submitted && repeat !== next ? t('system.account.password.mismatch') : undefined,
  })
  const general = $derived(apiErr && !apiErr.field ? errorText(apiErr) : '')

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    apiErr = null
    if (errors.current || errors.next || errors.repeat) return
    busy = true
    try {
      const kept = keepTokens
      await api.auth.changePassword({ currentPassword: current, newPassword: next, keepTokens: kept })
      current = ''
      next = ''
      repeat = ''
      keepTokens = false
      submitted = false
      toast.success(t(kept ? 'system.account.password.changedKeepTokens' : 'system.account.password.changed'))
      onchanged?.()
    } catch (err) {
      apiErr = toApiError(err)
    } finally {
      busy = false
    }
  }
</script>

<Panel title={t('system.account.password.title')} description={t('system.account.password.description')}>
  <form class="stack" onsubmit={submit} novalidate>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}
    <!-- Lets password managers attach the new password to the right account. -->
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
    <Field label={t('system.account.password.current')} error={errors.current}>
      <Input type="password" bind:value={current} autocomplete="current-password" maxlength={1024} required />
    </Field>
    <Field
      label={t('system.account.password.new')}
      error={errors.next}
      help={t('system.account.password.help', { min: MIN_PASSWORD })}
    >
      <Input type="password" bind:value={next} autocomplete="new-password" maxlength={1024} required />
    </Field>
    {#if next}<PasswordStrength password={next} />{/if}
    <Field label={t('system.account.password.repeat')} error={errors.repeat}>
      <Input type="password" bind:value={repeat} autocomplete="new-password" maxlength={1024} required />
    </Field>
    <Checkbox
      bind:checked={keepTokens}
      label={t('system.account.password.keepTokens')}
      description={t('system.account.password.keepTokensHelp')}
    />
    <div class="row">
      <Button type="submit" variant="primary" loading={busy}>
        {t('system.account.password.submit')}
      </Button>
    </div>
  </form>
</Panel>
