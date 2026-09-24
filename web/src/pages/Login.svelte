<!--
  @component
  Sign-in with an optional TOTP step (the server answers 401 with field
  "totp" when a code is required or wrong).
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { api, toApiError } from '../lib/api'
  import { errorText } from '../lib/errors'
  import { session } from '../lib/session.svelte'
  import { Button, Field, Input, Notice } from '../lib/ui'
  import AuthScreen from './auth/AuthScreen.svelte'

  let username = $state('')
  let password = $state('')
  let code = $state('')
  let step = $state<'password' | 'totp'>('password')
  let busy = $state(false)
  let error = $state('')
  let codeInput = $state<HTMLInputElement>()
  let userInput = $state<HTMLInputElement>()

  $effect(() => {
    if (step === 'totp') codeInput?.focus()
    else userInput?.focus()
  })

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    error = ''
    if (step === 'totp' && !/^\d{6}$/.test(code.trim())) {
      error = t('auth.login.codeFormat')
      return
    }
    busy = true
    try {
      await api.auth.login({ username: username.trim(), password, totp: step === 'totp' ? code.trim() : undefined })
      password = ''
      code = ''
      await session.signedIn()
    } catch (err) {
      const ae = toApiError(err)
      if (ae.code === 'unauthorized' && ae.field === 'totp') {
        if (step === 'totp') error = t('auth.login.codeWrong')
        step = 'totp'
        code = ''
      } else if (ae.code === 'unauthorized') {
        error = t('auth.login.wrong')
      } else if (ae.code === 'too_many_requests') {
        error = t('auth.login.throttled')
      } else {
        error = errorText(ae)
      }
    } finally {
      busy = false
    }
  }

  function back() {
    step = 'password'
    code = ''
    error = ''
  }
</script>

<AuthScreen title={step === 'totp' ? t('auth.login.totpTitle') : t('auth.login.title')}>
  {#if session.expired && step === 'password'}
    <Notice tone="info">{t('auth.login.expired')}</Notice>
  {/if}
  {#if error}
    <Notice tone="fail">{error}</Notice>
  {/if}

  <form class="stack" onsubmit={submit} novalidate>
    {#if step === 'password'}
      <Field label={t('auth.login.username')}>
        <Input
          bind:ref={userInput}
          bind:value={username}
          autocomplete="username"
          autocapitalize="off"
          spellcheck={false}
          maxlength={64}
          required
        />
      </Field>
      <Field label={t('auth.login.password')}>
        <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
      </Field>
      <Button type="submit" variant="primary" loading={busy} disabled={!username.trim() || !password}>
        {t('auth.login.submit')}
      </Button>
    {:else}
      <p class="muted">{t('auth.login.totpHelp')}</p>
      <Field label={t('auth.login.code')}>
        <Input
          bind:ref={codeInput}
          bind:value={code}
          inputmode="numeric"
          autocomplete="one-time-code"
          pattern="[0-9]*"
          maxlength={6}
          mono
          required
        />
      </Field>
      <Button type="submit" variant="primary" loading={busy} disabled={code.trim().length !== 6}>
        {t('auth.login.verify')}
      </Button>
      <Button variant="ghost" onclick={back} disabled={busy}>{t('auth.login.back')}</Button>
    {/if}
  </form>
</AuthScreen>
