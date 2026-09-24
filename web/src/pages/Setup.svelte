<!--
  @component
  First-run setup: the one-time setup token (with hints where to find it),
  the admin user name and a password entered twice, with a strength hint.
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { api, toApiError } from '../lib/api'
  import { errorText, fieldError } from '../lib/errors'
  import { session } from '../lib/session.svelte'
  import { Button, CopyButton, Field, Input, Notice } from '../lib/ui'
  import AuthScreen from './auth/AuthScreen.svelte'
  import { splitHint } from './auth/hints'
  import { passwordStrength } from './auth/strength'

  const MIN_PASSWORD = 10

  let token = $state('')
  let username = $state('admin')
  let password = $state('')
  let repeat = $state('')
  let busy = $state(false)
  let submitted = $state(false)
  let error = $state('')
  let apiErr = $state<unknown>(null)

  const hints = $derived((session.status?.setupHints ?? []).map(splitHint))
  // The server counts characters (runes), not UTF-16 code units.
  const passwordLength = $derived([...password].length)
  const strength = $derived(passwordStrength(password, MIN_PASSWORD))
  const strengthLabel = $derived(
    [
      t('auth.setup.strength.tooShort', { min: MIN_PASSWORD }),
      t('auth.setup.strength.weak'),
      t('auth.setup.strength.fair'),
      t('auth.setup.strength.good'),
      t('auth.setup.strength.strong'),
    ][strength],
  )

  const errors = $derived({
    token: fieldError(apiErr, 'setupToken') ?? (submitted && !token.trim() ? t('auth.setup.tokenRequired') : undefined),
    username:
      fieldError(apiErr, 'username') ?? (submitted && !username.trim() ? t('auth.setup.usernameRequired') : undefined),
    password:
      fieldError(apiErr, 'password') ??
      (submitted && passwordLength < MIN_PASSWORD ? t('auth.setup.passwordShort', { min: MIN_PASSWORD }) : undefined),
    repeat: submitted && repeat !== password ? t('auth.setup.mismatch') : undefined,
  })

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    apiErr = null
    error = ''
    if (errors.token || errors.username || errors.password || errors.repeat) return
    busy = true
    try {
      await api.auth.setup({ setupToken: token.trim(), username: username.trim(), password })
      password = ''
      repeat = ''
      await session.signedIn()
    } catch (err) {
      const ae = toApiError(err)
      apiErr = ae
      if (!ae.field) error = ae.code === 'forbidden' ? t('auth.setup.forbidden') : errorText(ae)
    } finally {
      busy = false
    }
  }
</script>

<AuthScreen title={t('auth.setup.title')} intro={t('auth.setup.intro')}>
  {#if error}<Notice tone="fail">{error}</Notice>{/if}

  <form class="stack" onsubmit={submit} novalidate>
    <Field label={t('auth.setup.token')} error={errors.token}>
      <Input bind:value={token} mono autocomplete="off" spellcheck={false} maxlength={256} required />
    </Field>

    <div class="hints">
      <p class="small muted">{t('auth.setup.hintsTitle')}</p>
      {#if hints.length > 0}
        <ul>
          {#each hints as hint, i (i)}
            <li>
              <span class="small">{hint.text}</span>
              {#if hint.command}
                <span class="cmd"><code>{hint.command}</code><CopyButton text={hint.command} /></span>
              {/if}
            </li>
          {/each}
        </ul>
      {:else}
        <p class="small muted">{t('auth.setup.hintsDefault')}</p>
      {/if}
    </div>

    <Field label={t('auth.setup.username')} error={errors.username}>
      <Input bind:value={username} autocomplete="username" autocapitalize="off" spellcheck={false} maxlength={64} required />
    </Field>

    <Field label={t('auth.setup.password')} error={errors.password} help={t('auth.setup.passwordHelp')}>
      <Input type="password" bind:value={password} autocomplete="new-password" maxlength={1024} required />
    </Field>
    {#if password}
      <div class="strength" aria-live="polite">
        <span class="bars" aria-hidden="true">
          {#each [1, 2, 3, 4] as i (i)}
            <span class={['bar', strength >= i && `s${strength}`]}></span>
          {/each}
        </span>
        <span class="small">{strengthLabel}</span>
      </div>
    {/if}

    <Field label={t('auth.setup.repeat')} error={errors.repeat}>
      <Input type="password" bind:value={repeat} autocomplete="new-password" maxlength={1024} required />
    </Field>

    <Button type="submit" variant="primary" loading={busy}>{t('auth.setup.submit')}</Button>
  </form>
</AuthScreen>

<style>
  .hints {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin-top: calc(-1 * var(--sp-2));
    padding: var(--sp-3);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  ul {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .cmd {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2);
    min-width: 0;
    padding-left: var(--sp-2);
    border-left: 2px solid var(--line-strong);
  }
  code {
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  .strength {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    margin-top: calc(-1 * var(--sp-2));
  }
  .bars {
    display: inline-flex;
    gap: 4px;
  }
  .bar {
    width: 28px;
    height: 6px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
  }
  .s1 {
    background: var(--fail);
  }
  .s2 {
    background: var(--warn);
  }
  .s3,
  .s4 {
    background: var(--ok);
  }
</style>
