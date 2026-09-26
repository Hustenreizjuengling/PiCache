<!--
  @component
  One change to an account, confirmed with the admin's own password: add a
  user (name, password, role; viewer by default), change a role (demoting
  yourself signs you out), reset another user's password (signs them out
  and revokes their API tokens), turn off another user's two-factor
  authentication (signs them out) or delete an account. Field errors appear
  at their inputs; the server's limits (at least one admin, at most 32
  accounts) in the dialog. Mount it only while it is open ({#if}), so every
  opening starts empty and the passwords are dropped when it closes.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type Role, type User } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Dialog, Field, Input, Notice, Select } from '$lib/ui'
  import { charCount, MIN_PASSWORD } from '../forms'
  import PasswordStrength from './PasswordStrength.svelte'
  import { USERNAME, type UserAction } from './users'

  interface Props {
    open?: boolean
    action: UserAction
    /** The account to change (all actions but create). */
    user?: User
    /** The account is the signed-in one. */
    self?: boolean
    /** After the change succeeded (the dialog closes): the new state of the account, if any. */
    ondone: (user: User | undefined) => void
  }

  let { open = $bindable(true), action, user, self = false, ondone }: Props = $props()

  let username = $state('')
  let password = $state('')
  let role = $state<Role>(untrack(() => user?.role ?? 'viewer'))
  let current = $state('')
  let submitted = $state(false)
  let busy = $state(false)
  let apiErr = $state.raw<ApiError | null>(null)

  const name = $derived(user?.username ?? '')
  const demotingSelf = $derived(action === 'role' && self && role === 'viewer' && user?.role === 'admin')

  const errors = $derived({
    username:
      fieldError(apiErr, 'username') ??
      (action === 'create' && submitted
        ? !username.trim()
          ? t('system.users.usernameRequired')
          : !USERNAME.test(username.trim())
            ? t('system.users.usernameRule')
            : undefined
        : undefined),
    password:
      fieldError(apiErr, 'password') ??
      ((action === 'create' || action === 'password') && submitted && charCount(password) < MIN_PASSWORD
        ? t('system.account.password.tooShort', { min: MIN_PASSWORD })
        : undefined),
    role: fieldError(apiErr, 'role'),
    current:
      fieldError(apiErr, 'currentPassword') ?? (submitted && !current ? t('system.users.currentRequired') : undefined),
  })
  /** The fields this action shows (server errors naming others appear above the form). */
  const inputs = $derived(
    {
      create: ['username', 'password', 'role', 'currentPassword'],
      role: ['role', 'currentPassword'],
      password: ['password', 'currentPassword'],
      totp: ['currentPassword'],
      delete: ['currentPassword'],
    }[action],
  )
  // 409 (last admin, 32 accounts, name taken), "body", "disableTotp" and anything else without its own input.
  const general = $derived(apiErr && !(apiErr.field && inputs.includes(apiErr.field)) ? errorText(apiErr) : '')
  const unchanged = $derived(action === 'role' && role === user?.role)

  const roleOptions = $derived([
    { value: 'viewer', label: t('common.account.role.viewer') },
    { value: 'admin', label: t('common.account.role.admin') },
  ])

  const title = $derived(
    {
      create: t('system.users.addTitle'),
      role: t('system.users.roleTitle', { name }),
      password: t('system.users.passwordTitle', { name }),
      totp: t('system.users.totpTitle', { name }),
      delete: self ? t('system.users.deleteSelfTitle') : t('system.users.deleteTitle', { name }),
    }[action],
  )
  const submitLabel = $derived(
    {
      create: t('system.users.add'),
      role: t('system.users.changeRole'),
      password: t('system.users.resetPassword'),
      totp: t('system.users.disableTotp'),
      delete: t('system.users.delete'),
    }[action],
  )

  async function run(): Promise<User | undefined> {
    const currentPassword = current
    switch (action) {
      case 'create':
        return api.users.create({ username: username.trim(), password, role, currentPassword })
      case 'role':
        return api.users.update(user!.id, { role, currentPassword })
      case 'password':
        return api.users.update(user!.id, { password, currentPassword })
      case 'totp':
        return api.users.update(user!.id, { disableTotp: true, currentPassword })
      case 'delete':
        await api.users.remove(user!.id, currentPassword)
        return undefined
    }
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    apiErr = null
    if (errors.username || errors.password || errors.current || unchanged) return
    busy = true
    try {
      const result = await run()
      password = ''
      current = ''
      open = false
      ondone(result)
    } catch (err) {
      apiErr = toApiError(err)
    } finally {
      busy = false
    }
  }

  const formId = $props.id()
</script>

<Dialog bind:open {title} size="sm" dismissible={!busy}>
  <form id="user-{formId}" class="stack" onsubmit={submit} novalidate>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}

    {#if action === 'create'}
      <Field label={t('system.users.username')} error={errors.username} help={t('system.users.usernameHelp')}>
        <Input bind:value={username} autocomplete="off" autocapitalize="off" spellcheck={false} maxlength={64} required />
      </Field>
    {/if}

    {#if action === 'create' || action === 'password'}
      <Field
        label={action === 'create' ? t('system.users.password') : t('system.users.newPassword')}
        error={errors.password}
        help={t('system.account.password.help', { min: MIN_PASSWORD })}
      >
        <Input type="password" bind:value={password} autocomplete="new-password" maxlength={1024} required />
      </Field>
      {#if password}<PasswordStrength {password} />{/if}
    {/if}

    {#if action === 'create' || action === 'role'}
      <Field
        label={t('system.users.role')}
        error={errors.role}
        help={role === 'admin' ? t('system.users.roleHelp.admin') : t('system.users.roleHelp.viewer')}
      >
        <Select options={roleOptions} bind:value={() => role, (v) => (role = v === 'admin' ? 'admin' : 'viewer')} />
      </Field>
    {/if}

    {#if demotingSelf}
      <Notice tone="warn">{t('system.users.signedOut')}</Notice>
    {:else if action === 'password'}
      <p class="small muted">{t('system.users.passwordNote')}</p>
    {:else if action === 'totp'}
      <p class="small muted">{t('system.users.totpNote')}</p>
    {:else if action === 'delete'}
      <p class="small muted">{t('system.users.deleteText', { name })}</p>
      {#if self}<Notice tone="warn">{t('system.users.signedOut')}</Notice>{/if}
    {/if}

    <!-- Lets password managers offer the signed-in account's password (not the new user's). -->
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
    <Field label={t('system.users.current')} error={errors.current} help={t('system.users.currentHelp')}>
      <Input type="password" bind:value={current} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={busy} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button
      type="submit"
      form="user-{formId}"
      variant={action === 'delete' || action === 'totp' ? 'danger' : 'primary'}
      loading={busy}
      disabled={unchanged}
    >
      {submitLabel}
    </Button>
  {/snippet}
</Dialog>
