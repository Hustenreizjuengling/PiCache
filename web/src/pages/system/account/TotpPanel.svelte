<!--
  @component
  Two-factor authentication (TOTP): shows whether it is on, sets it up
  (password first, then QR code drawn in the browser, manual key,
  confirmation code; the server then signs out the other sessions) and turns
  it off (password required). The secret is forgotten when the dialog closes.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, isApiError, toApiError, type ApiError, type TotpBegin, type User } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, CopyButton, Dialog, Field, Input, Notice, Panel, Skeleton, toast } from '$lib/ui'
  import QrCode from '../QrCode.svelte'

  let { user, onchange }: { user?: User; onchange: () => void } = $props()

  // ---- setup
  let setupOpen = $state(false)
  let starting = $state(false)
  let pending = $state.raw<TotpBegin | undefined>(undefined)
  let code = $state('')
  let codeSubmitted = $state(false)
  let confirming = $state(false)
  let setupErr = $state.raw<ApiError | null>(null)
  /** The pending setup expired or was replaced (409): offer to start again. */
  const setupStale = $derived(isApiError(setupErr, 'conflict'))

  const cleanCode = $derived(code.replace(/\s+/g, ''))
  const codeError = $derived(
    fieldError(setupErr, 'code') ??
      (codeSubmitted && !/^\d{6}$/.test(cleanCode) ? t('system.account.totp.codeFormat') : undefined),
  )
  const setupGeneral = $derived(setupErr && !setupErr.field ? errorText(setupErr) : '')
  const groupedSecret = $derived(pending ? (pending.secret.match(/.{1,4}/g) ?? []).join(' ') : '')

  // ---- password before setup (a stolen session must not enrol its own authenticator)
  let passwordOpen = $state(false)
  let setupPassword = $state('')
  let passwordSubmitted = $state(false)
  let beginErr = $state.raw<ApiError | null>(null)
  const setupPasswordError = $derived(
    fieldError(beginErr, 'currentPassword') ??
      (passwordSubmitted && !setupPassword ? t('system.account.totp.passwordRequired') : undefined),
  )
  const beginGeneral = $derived(beginErr && !beginErr.field ? errorText(beginErr) : '')

  /** Asks for the password (again, e.g. after the pending setup expired). */
  function openBegin() {
    setupPassword = ''
    passwordSubmitted = false
    beginErr = null
    setupOpen = false
    passwordOpen = true
  }

  async function begin(e: SubmitEvent) {
    e.preventDefault()
    passwordSubmitted = true
    beginErr = null
    if (!setupPassword) return
    starting = true
    try {
      pending = await api.auth.totpBegin(setupPassword)
      setupPassword = ''
      code = ''
      codeSubmitted = false
      setupErr = null
      passwordOpen = false
      setupOpen = true
    } catch (err) {
      beginErr = toApiError(err)
    } finally {
      starting = false
    }
  }

  function passwordClosed() {
    setupPassword = ''
    passwordSubmitted = false
  }

  async function confirmSetup(e: SubmitEvent) {
    e.preventDefault()
    codeSubmitted = true
    setupErr = null
    if (!/^\d{6}$/.test(cleanCode)) return
    confirming = true
    try {
      await api.auth.totpConfirm(cleanCode)
      setupOpen = false
      toast.success(t('system.account.totp.enabled'))
      onchange()
    } catch (err) {
      setupErr = toApiError(err)
    } finally {
      confirming = false
    }
  }

  function setupClosed() {
    pending = undefined
    code = ''
    codeSubmitted = false
    setupErr = null
  }

  // ---- disable
  let disableOpen = $state(false)
  let password = $state('')
  let disableSubmitted = $state(false)
  let disabling = $state(false)
  let disableErr = $state.raw<ApiError | null>(null)
  const passwordError = $derived(
    fieldError(disableErr, 'password') ??
      (disableSubmitted && !password ? t('system.account.totp.passwordRequired') : undefined),
  )
  const disableGeneral = $derived(disableErr && !disableErr.field ? errorText(disableErr) : '')

  function openDisable() {
    password = ''
    disableSubmitted = false
    disableErr = null
    disableOpen = true
  }

  async function disable(e: SubmitEvent) {
    e.preventDefault()
    disableSubmitted = true
    disableErr = null
    if (!password) return
    disabling = true
    try {
      await api.auth.totpDisable(password)
      disableOpen = false
      toast.success(t('system.account.totp.disabled'))
      onchange()
    } catch (err) {
      disableErr = toApiError(err)
    } finally {
      disabling = false
    }
  }

  const setupFormId = $props.id()
</script>

<Panel title={t('system.account.totp.title')} description={t('system.account.totp.description')}>
  {#if !user}
    <Skeleton height="64px" />
  {:else}
    <div class="stack">
      <div class="status">
        {#if user.totpEnabled}
          <Chip tone="ok" icon="shield-check" label={t('system.account.totp.on')} />
          <p class="small muted">{t('system.account.totp.onText')}</p>
        {:else}
          <Chip tone="warn" icon="shield-off" label={t('system.account.totp.off')} />
          <p class="small muted">{t('system.account.totp.offText')}</p>
        {/if}
      </div>
      <div class="row">
        {#if user.totpEnabled}
          <Button icon="shield-off" onclick={openDisable}>
            {t('system.account.totp.disable')}
          </Button>
        {:else}
          <Button icon="shield-check" onclick={openBegin}>
            {t('system.account.totp.setup')}
          </Button>
        {/if}
      </div>
    </div>
  {/if}
</Panel>

<Dialog
  bind:open={passwordOpen}
  title={t('system.account.totp.passwordTitle')}
  size="sm"
  dismissible={!starting}
  onclose={passwordClosed}
>
  <form id="totp-pw-{setupFormId}" class="stack" onsubmit={begin} novalidate>
    <p class="small muted">{t('system.account.totp.passwordText')}</p>
    {#if beginGeneral}<Notice tone="fail">{beginGeneral}</Notice>{/if}
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
    <Field label={t('system.account.totp.password')} error={setupPasswordError}>
      <Input type="password" bind:value={setupPassword} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={starting} onclick={() => (passwordOpen = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="totp-pw-{setupFormId}" variant="primary" loading={starting}>
      {t('system.account.totp.continue')}
    </Button>
  {/snippet}
</Dialog>

<Dialog bind:open={setupOpen} title={t('system.account.totp.setupTitle')} dismissible={!confirming} onclose={setupClosed}>
  {#if pending}
    <form id="totp-{setupFormId}" class="stack" onsubmit={confirmSetup} novalidate>
      {#if setupStale}
        <Notice tone="warn" title={setupGeneral}>
          {#snippet actions()}
            <Button size="sm" icon="refresh" onclick={openBegin}>{t('system.account.totp.restart')}</Button>
          {/snippet}
        </Notice>
      {:else if setupGeneral}
        <Notice tone="fail">{setupGeneral}</Notice>
      {/if}

      <section class="step">
        <h3>{t('system.account.totp.step1')}</h3>
        <p class="small muted">{t('system.account.totp.step1Text')}</p>
        <div class="scan">
          <QrCode
            text={pending.uri}
            label={t('system.account.totp.qrLabel')}
            fallback={t('system.account.totp.qrTooLong')}
          />
          <div class="manual">
            <p class="small">{t('system.account.totp.manual')}</p>
            <div class="secret">
              <code class="mono">{groupedSecret}</code>
              <CopyButton text={pending.secret} label={t('system.account.totp.copyKey')} />
            </div>
            <p class="xsmall subtle">{t('system.account.totp.params')}</p>
          </div>
        </div>
      </section>

      <section class="step">
        <h3>{t('system.account.totp.step2')}</h3>
        <Field label={t('system.account.totp.code')} error={codeError} help={t('system.account.totp.codeHelp')}>
          <Input
            bind:value={code}
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength={9}
            mono
            required
          />
        </Field>
      </section>
    </form>
  {/if}
  {#snippet actions()}
    <Button disabled={confirming} onclick={() => (setupOpen = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="totp-{setupFormId}" variant="primary" loading={confirming} disabled={setupStale}>
      {t('system.account.totp.confirm')}
    </Button>
  {/snippet}
</Dialog>

<Dialog
  bind:open={disableOpen}
  title={t('system.account.totp.disableTitle')}
  size="sm"
  dismissible={!disabling}
>
  <form id="totp-off-{setupFormId}" class="stack" onsubmit={disable} novalidate>
    <p class="small muted">{t('system.account.totp.disableText')}</p>
    {#if disableGeneral}<Notice tone="fail">{disableGeneral}</Notice>{/if}
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
    <Field label={t('system.account.totp.password')} error={passwordError}>
      <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={disabling} onclick={() => (disableOpen = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="totp-off-{setupFormId}" variant="danger" loading={disabling}>
      {t('system.account.totp.disable')}
    </Button>
  {/snippet}
</Dialog>

<style>
  .status {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
  .step {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .scan {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-4);
  }
  .manual {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    flex: 1 1 200px;
    min-width: 0;
  }
  .secret {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .secret code {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-md);
    letter-spacing: 0.04em;
  }
</style>
