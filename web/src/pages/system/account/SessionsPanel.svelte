<!--
  @component
  Signed-in browser sessions of this account: which device, from where,
  when it expires. Other sessions can be signed out one by one or all at
  once; the current one signs out through the regular sign-out.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, isApiError, resource, type SessionInfo } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, IconButton, KeyValue, Panel, SidePanel, Table, confirm, toast, type Column } from '$lib/ui'
  import { describeUserAgent } from '../useragent'

  /** Changing this value reloads the list (e.g. after a password change). */
  let { reload = 0 }: { reload?: number } = $props()

  const list = resource(
    (signal) => {
      void reload
      return api.auth.sessions({ signal })
    },
    { interval: 60_000 },
  )

  const others = $derived((list.data ?? []).filter((s) => !s.current))
  let selectedId = $state<string | undefined>(undefined)
  const selected = $derived(list.data?.find((s) => s.id === selectedId))
  let panelOpen = $state(false)

  function device(s: SessionInfo): string {
    const d = describeUserAgent(s.userAgent)
    if (d.browser && d.os) return t('system.account.sessions.device', { browser: d.browser, os: d.os })
    return d.browser || d.os || t('system.account.sessions.unknownDevice')
  }

  function open(s: SessionInfo) {
    selectedId = s.id
    panelOpen = true
  }

  async function revoke(s: SessionInfo) {
    if (s.current) {
      await session.logout()
      return
    }
    const ok = await confirm({
      title: t('system.account.sessions.revokeTitle', { device: device(s) }),
      message: t('system.account.sessions.revokeText', { ip: s.ip || '–' }),
      confirmLabel: t('system.account.sessions.revoke'),
      action: async () => {
        try {
          await api.auth.revokeSession(s.id)
        } catch (err) {
          if (!isApiError(err, 'not_found')) throw err // already gone: fine
        }
      },
    })
    if (!ok) return
    panelOpen = false
    toast.success(t('system.account.sessions.revoked'))
    void list.refresh()
  }

  async function revokeOthers() {
    const targets = others
    const ok = await confirm({
      title: t('system.account.sessions.revokeAllTitle'),
      message: tn('system.account.sessions.revokeAllText', targets.length),
      confirmLabel: t('system.account.sessions.revokeAll'),
      action: async () => {
        for (const s of targets) {
          try {
            await api.auth.revokeSession(s.id)
          } catch (err) {
            if (!isApiError(err, 'not_found')) throw err
          }
        }
      },
    })
    if (!ok) return
    toast.success(t('system.account.sessions.revokedAll'))
    void list.refresh()
  }

  const columns = $derived<Column<SessionInfo>[]>([
    { key: 'device', label: t('system.account.sessions.colDevice'), cell: deviceCell, value: (s) => device(s) },
    { key: 'ip', label: t('system.account.sessions.colIp'), cell: ipCell, value: (s) => s.ip },
    {
      key: 'lastSeen',
      label: t('system.account.sessions.colLastSeen'),
      value: (s) => Date.parse(s.lastSeen),
      format: (s) => formatRelative(s.lastSeen),
      sortable: true,
    },
    {
      key: 'created',
      label: t('system.account.sessions.colCreated'),
      value: (s) => Date.parse(s.createdAt),
      format: (s) => formatDateTime(s.createdAt),
      sortable: true,
    },
    {
      key: 'expires',
      label: t('system.account.sessions.colExpires'),
      value: (s) => Date.parse(s.expiresAt),
      format: (s) => formatRelative(s.expiresAt),
    },
    { key: 'actions', label: t('common.label.actions'), align: 'right', cell: actionsCell, width: '64px' },
  ])
</script>

{#snippet deviceCell(s: SessionInfo)}
  <span class="device">
    <span class="truncate" title={s.userAgent}>{device(s)}</span>
    {#if s.current}<Badge tone="info">{t('system.account.sessions.current')}</Badge>{/if}
  </span>
{/snippet}

{#snippet ipCell(s: SessionInfo)}
  <span class="mono nowrap">{s.ip || '–'}</span>
{/snippet}

{#snippet actionsCell(s: SessionInfo)}
  {#if !s.current}
    <IconButton
      icon="logout"
      size="sm"
      label={t('system.account.sessions.revokeNamed', { device: device(s) })}
      disabled={!session.isAdmin}
      onclick={() => revoke(s)}
    />
  {/if}
{/snippet}

<Panel title={t('system.account.sessions.title')} description={t('system.account.sessions.description')} flush>
  {#snippet actions()}
    <Button size="sm" icon="logout" disabled={others.length === 0 || !session.isAdmin} onclick={revokeOthers}>
      {t('system.account.sessions.revokeAll')}
    </Button>
  {/snippet}
  <Table
    {columns}
    rows={list.data}
    key={(s) => s.id}
    loading={list.loading}
    error={list.error && !list.data ? errorText(list.error) : undefined}
    onretry={() => list.refresh()}
    onrowclick={open}
    selected={panelOpen ? selectedId : undefined}
    caption={t('system.account.sessions.title')}
    emptyText={t('system.account.sessions.empty')}
    skeletonRows={2}
  />
</Panel>

<SidePanel bind:open={panelOpen} title={selected ? device(selected) : t('system.account.sessions.title')}>
  {#if selected}
    <div class="stack">
      {#if selected.current}
        <p class="small muted">{t('system.account.sessions.currentText')}</p>
      {/if}
      <KeyValue
        items={[
          { label: t('system.account.sessions.colIp'), value: selected.ip, mono: true },
          { label: t('system.account.sessions.colCreated'), value: formatDateTime(selected.createdAt) },
          { label: t('system.account.sessions.colLastSeen'), value: formatDateTime(selected.lastSeen) },
          { label: t('system.account.sessions.colExpires'), value: formatDateTime(selected.expiresAt) },
          { label: t('system.account.sessions.userAgent'), value: selected.userAgent, mono: true },
        ]}
      />
    </div>
  {:else}
    <p class="small muted">{t('system.account.sessions.gone')}</p>
  {/if}
  {#snippet actions()}
    {#if selected}
      <Button variant="danger" icon="logout" disabled={!session.isAdmin} onclick={() => revoke(selected)}>
        {selected.current ? t('common.account.logout') : t('system.account.sessions.revoke')}
      </Button>
    {/if}
  {/snippet}
</SidePanel>

<style>
  .device {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    max-width: 100%;
    min-width: 0;
  }
</style>
