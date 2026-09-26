<!--
  @component
  The accounts (admins only): name, role, two-factor authentication,
  created and last sign-in, "Add user" (at most 32 accounts) and per row
  Change role, Reset password, Disable two-factor (another user's, when
  on) and Delete (where the host allows destructive actions). Every change
  asks for the admin's password (UserDialog). Read-only while the host
  locks the configuration. Changing your own role or deleting yourself
  ends your sessions: the app returns to the sign-in page.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type User } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDate, formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Chip, Menu, Panel, Table, toast, type Column, type MenuItem } from '$lib/ui'
  import UserDialog from './UserDialog.svelte'
  import { MAX_ACCOUNTS, type UserAction } from './users'

  /** Called after a change to any account (the page reloads its own account). */
  let { onchanged }: { onchanged?: () => void } = $props()

  const users = resource((signal) => api.users.list({ signal }))
  const full = $derived((users.data?.length ?? 0) >= MAX_ACCOUNTS)
  const selfId = $derived(session.user?.id)

  let dialog = $state.raw<{ action: UserAction; user?: User } | undefined>(undefined)
  let dialogOpen = $state(false)

  function open(action: UserAction, user?: User) {
    dialog = { action, user }
    dialogOpen = true
  }

  async function done(action: UserAction, user: User | undefined, result: User | undefined) {
    const self = !!user && user.id === selfId
    // Your own role change or deletion ended your sessions (the cookies are cleared).
    if (self && (action === 'role' || action === 'delete')) {
      toast.success(t(action === 'role' ? 'system.users.selfRoleChanged' : 'system.users.selfDeleted'))
      await session.load()
      return
    }
    const name = result?.username ?? user?.username ?? ''
    const messages: Record<UserAction, string> = {
      create: t('system.users.created', { name }),
      role: t('system.users.roleChanged', { name }),
      password: t('system.users.passwordReset', { name }),
      totp: t('system.users.totpDisabled', { name }),
      delete: t('system.users.deleted', { name }),
    }
    toast.success(messages[action])
    void users.refresh()
    onchanged?.()
  }

  function items(u: User): MenuItem[] {
    const self = u.id === selfId
    const list: MenuItem[] = [{ label: t('system.users.changeRole'), icon: 'user', onselect: () => open('role', u) }]
    // Your own password and two-factor authentication are changed above ("Your account").
    if (!self) list.push({ label: t('system.users.resetPassword'), icon: 'key', onselect: () => open('password', u) })
    if (!self && u.totpEnabled) {
      list.push({ label: t('system.users.disableTotp'), icon: 'shield-off', onselect: () => open('totp', u) })
    }
    if (session.canDestroy) {
      list.push({ separator: true }, { label: t('system.users.delete'), icon: 'trash', danger: true, onselect: () => open('delete', u) })
    }
    return list
  }

  const columns = $derived<Column<User>[]>([
    { key: 'username', label: t('system.users.username'), cell: nameCell, value: (u) => u.username, sortable: true, width: '30%' },
    { key: 'role', label: t('system.users.role'), cell: roleCell, value: (u) => u.role, sortable: true },
    { key: 'totp', label: t('system.users.totp'), cell: totpCell, value: (u) => (u.totpEnabled ? 1 : 0), sortable: true },
    {
      key: 'created',
      label: t('common.label.created'),
      sortable: true,
      cell: createdCell,
      value: (u) => Date.parse(u.createdAt),
    },
    {
      key: 'lastLogin',
      label: t('system.users.lastLogin'),
      sortable: true,
      cell: lastLoginCell,
      value: (u) => (u.lastLoginAt ? Date.parse(u.lastLoginAt) : undefined),
    },
    ...(session.isAdmin
      ? [{ key: 'actions', label: t('common.label.actions'), align: 'right', width: '1%', cell: actionsCell } satisfies Column<User>]
      : []),
  ])
</script>

{#snippet nameCell(u: User)}
  <span class="name">
    <span class="truncate" title={u.username}>{u.username}</span>
    {#if u.id === selfId}<Badge tone="info">{t('system.users.you')}</Badge>{/if}
  </span>
{/snippet}

{#snippet roleCell(u: User)}
  <Badge tone={u.role === 'admin' ? 'warn' : 'neutral'}>{t(`common.account.role.${u.role}`)}</Badge>
{/snippet}

{#snippet totpCell(u: User)}
  {#if u.totpEnabled}
    <Chip size="sm" tone="ok" icon="shield-check" label={t('common.state.on')} />
  {:else}
    <Chip size="sm" tone="neutral" icon="shield-off" label={t('common.state.off')} />
  {/if}
{/snippet}

{#snippet createdCell(u: User)}
  <span class="nowrap" title={formatDateTime(u.createdAt)}>{formatDate(u.createdAt)}</span>
{/snippet}

{#snippet lastLoginCell(u: User)}
  {#if u.lastLoginAt}
    <span class="nowrap" title={formatDateTime(u.lastLoginAt)}>{formatRelative(u.lastLoginAt)}</span>
  {:else}
    <span class="subtle">{t('common.state.never')}</span>
  {/if}
{/snippet}

{#snippet actionsCell(u: User)}
  <Menu iconOnly icon="more" variant="ghost" size="sm" label={t('system.users.actionsNamed', { name: u.username })} items={items(u)} />
{/snippet}

{#snippet limitNote()}
  <p class="small muted">{t('system.users.limit', { max: MAX_ACCOUNTS })}</p>
{/snippet}

<Panel id="users" title={t('system.users.title')} description={t('system.users.description')} flush footer={full ? limitNote : undefined}>
  {#snippet actions()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin || full || !users.data} onclick={() => open('create')}>
      {t('system.users.add')}
    </Button>
  {/snippet}
  <Table
    {columns}
    rows={users.data}
    key={(u) => u.id}
    loading={users.loading}
    error={users.error && !users.data ? errorText(users.error) : undefined}
    onretry={() => users.refresh()}
    caption={t('system.users.title')}
    skeletonRows={2}
  />
</Panel>

{#if dialogOpen && dialog}
  {@const d = dialog}
  <UserDialog
    bind:open={dialogOpen}
    action={d.action}
    user={d.user}
    self={!!d.user && d.user.id === selfId}
    ondone={(result) => done(d.action, d.user, result)}
  />
{/if}

<style>
  .name {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    max-width: 100%;
    min-width: 0;
  }
</style>
