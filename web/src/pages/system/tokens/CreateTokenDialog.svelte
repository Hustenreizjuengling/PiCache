<!--
  @component
  Creates an API token and shows its secret exactly once. Creating one needs
  the current password (a token outlives the session). Mount it only while
  it is open ({#if}), so every opening starts with an empty form and the
  secret and the password are dropped from memory when it closes.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, apiUrl, toApiError, type ApiError, type CreatedToken, type Scope, type TokenInfo } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDate } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, CopyButton, Dialog, Field, Input, Notice, Select } from '$lib/ui'
  import { charCount, MAX_TOKEN_NAME, rangeError, RANGES } from '../forms'

  interface Props {
    open?: boolean
    oncreated?: (info: TokenInfo) => void
  }

  let { open = $bindable(true), oncreated }: Props = $props()

  type Expiry = '30' | '90' | '365' | 'never' | 'custom'

  let name = $state('')
  let scope = $state<Scope>('read')
  let expiry = $state<Expiry>('365')
  let customDays = $state<number | null>(180)
  let password = $state('')
  let submitted = $state(false)
  let busy = $state(false)
  let apiErr = $state.raw<ApiError | null>(null)
  let created = $state.raw<CreatedToken | undefined>(undefined)

  const CONTROL_CHARS = /[\u0000-\u001f\u007f-\u009f]/

  function nameError(): string | undefined {
    const n = name.trim()
    if (!n) return submitted ? t('system.tokens.nameRequired') : undefined
    if (charCount(n) > MAX_TOKEN_NAME) return t('system.tokens.nameTooLong', { max: MAX_TOKEN_NAME })
    if (CONTROL_CHARS.test(n)) return t('system.tokens.nameControl')
    return undefined
  }

  const days = $derived(expiry === 'never' ? 0 : expiry === 'custom' ? customDays : Number(expiry))
  const errors = $derived({
    name: fieldError(apiErr, 'name') ?? nameError(),
    scope: fieldError(apiErr, 'scope'),
    days:
      fieldError(apiErr, 'expiresInDays') ??
      (expiry === 'custom' ? rangeError(customDays, RANGES.tokenExpiryDays) : undefined),
    password:
      fieldError(apiErr, 'currentPassword') ??
      (submitted && !password ? t('system.tokens.passwordRequired') : undefined),
  })
  const general = $derived(apiErr && !apiErr.field ? errorText(apiErr) : '')

  const scopeOptions = $derived([
    { value: 'read', label: t('system.tokens.scope.read') },
    { value: 'admin', label: t('system.tokens.scope.admin') },
  ])
  const expiryOptions = $derived([
    { value: '30', label: t('system.tokens.expiry.days', { days: 30 }) },
    { value: '90', label: t('system.tokens.expiry.days', { days: 90 }) },
    { value: '365', label: t('system.tokens.expiry.year') },
    { value: 'never', label: t('system.tokens.expiry.never') },
    { value: 'custom', label: t('system.tokens.expiry.custom') },
  ])

  const example = $derived(
    `curl -fsS -H "Authorization: Bearer $PICACHE_TOKEN" ${apiUrl('/system/health')}`,
  )

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    apiErr = null
    if (errors.name || errors.days || errors.password) return
    busy = true
    try {
      const body = { name: name.trim(), scope, currentPassword: password, ...(days ? { expiresInDays: days } : {}) }
      created = await api.tokens.create(body)
      password = ''
      oncreated?.(created.info)
    } catch (err) {
      apiErr = toApiError(err)
    } finally {
      busy = false
    }
  }

  const formId = $props.id()
</script>

<Dialog
  bind:open
  title={created ? t('system.tokens.createdTitle') : t('system.tokens.createTitle')}
  subtitle={created ? created.info.name : undefined}
  dismissible={!busy}
>
  {#if created}
    <div class="stack">
      <Notice tone="warn" title={t('system.tokens.onceTitle')}>{t('system.tokens.onceText')}</Notice>
      <div class="secret">
        <code class="mono">{created.token}</code>
        <CopyButton text={created.token} showLabel size="md" label={t('system.tokens.copy')} />
      </div>
      <p class="small muted">
        {created.info.scope === 'admin' ? t('system.tokens.scopeHelp.admin') : t('system.tokens.scopeHelp.read')}
        {created.info.expiresAt
          ? t('system.tokens.expiresOn', { date: formatDate(created.info.expiresAt) })
          : t('system.tokens.neverExpires')}
      </p>
      <div class="stack-sm">
        <p class="small">{t('system.tokens.exampleTitle')}</p>
        <div class="example">
          <code class="mono">{example}</code>
          <CopyButton text={example} />
        </div>
      </div>
    </div>
  {:else}
    <form id="token-{formId}" class="stack" onsubmit={submit} novalidate>
      {#if general}<Notice tone="fail">{general}</Notice>{/if}
      <Field label={t('system.tokens.name')} error={errors.name} help={t('system.tokens.nameHelp')}>
        <Input bind:value={name} maxlength={MAX_TOKEN_NAME} autocomplete="off" required />
      </Field>
      <Field
        label={t('system.tokens.scopeLabel')}
        error={errors.scope}
        help={scope === 'admin' ? t('system.tokens.scopeHelp.admin') : t('system.tokens.scopeHelp.read')}
      >
        <Select options={scopeOptions} bind:value={() => scope, (v) => (scope = v === 'admin' ? 'admin' : 'read')} />
      </Field>
      <Field label={t('system.tokens.expiryLabel')} error={expiry === 'custom' ? undefined : errors.days}>
        <Select options={expiryOptions} bind:value={() => expiry, (v) => (expiry = v as Expiry)} />
      </Field>
      {#if expiry === 'custom'}
        <Field label={t('system.tokens.customDays')} error={errors.days}>
          <Input
            type="number"
            bind:value={customDays}
            min={RANGES.tokenExpiryDays.min}
            max={RANGES.tokenExpiryDays.max}
            step={1}
            inputmode="numeric"
          />
        </Field>
      {/if}
      {#if expiry === 'never'}
        <p class="small muted">{t('system.tokens.neverHint')}</p>
      {/if}
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
      <Field label={t('system.tokens.password')} error={errors.password} help={t('system.tokens.passwordHelp')}>
        <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
      </Field>
    </form>
  {/if}
  {#snippet actions()}
    {#if created}
      <Button variant="primary" onclick={() => (open = false)}>{t('system.tokens.done')}</Button>
    {:else}
      <Button disabled={busy} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
      <Button type="submit" form="token-{formId}" variant="primary" icon="key" loading={busy}>
        {t('system.tokens.create')}
      </Button>
    {/if}
  {/snippet}
</Dialog>

<style>
  .secret,
  .example {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .secret code,
  .example code {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
    user-select: all;
  }
  .example code {
    font-size: var(--fs-sm);
  }
</style>
