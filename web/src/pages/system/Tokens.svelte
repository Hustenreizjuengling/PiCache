<!--
  @component
  API tokens for scripts, monitoring and automated backups: list, create
  (the secret is shown once), delete; plus the Prometheus metrics switch.
  Every session manages tokens (they are created for the signed-in account):
  viewers see and create their own read tokens, admins see every account's
  tokens with their owner and may delete any of them.
  Query: ?token=<id> opens a token's details.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type TokenInfo } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDate, formatDateTime, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import {
    Badge,
    Button,
    EmptyState,
    IconButton,
    KeyValue,
    Notice,
    Panel,
    SidePanel,
    Table,
    confirm,
    toast,
    type Column,
  } from '$lib/ui'
  import CreateTokenDialog from './tokens/CreateTokenDialog.svelte'
  import MetricsPanel from './tokens/MetricsPanel.svelte'

  const tokens = resource((signal) => api.tokens.list({ signal }))
  let createOpen = $state(false)

  const selectedId = $derived(Number(router.param('token')) || undefined)
  const selected = $derived(tokens.data?.find((x) => x.id === selectedId))

  function expired(tk: TokenInfo): boolean {
    return !!tk.expiresAt && Date.parse(tk.expiresAt) <= Date.now()
  }

  function select(tk: TokenInfo | undefined) {
    router.setQuery({ token: tk?.id }, { push: !!tk })
  }

  /** Admins list every account's tokens: show whose each one is. */
  const showOwner = $derived(session.canOperate)
  const mine = (tk: TokenInfo) => tk.userId === session.user?.id

  async function remove(tk: TokenInfo) {
    const ok = await confirm({
      title: t('system.tokens.deleteTitle', { name: tk.name }),
      message: mine(tk) ? t('system.tokens.deleteText') : t('system.tokens.deleteTextOther', { user: tk.username }),
      confirmLabel: t('system.tokens.delete'),
      action: () => api.tokens.remove(tk.id),
    })
    if (!ok) return
    if (selectedId === tk.id) select(undefined)
    toast.success(t('system.tokens.deleted'))
    void tokens.refresh()
  }

  function created() {
    toast.success(t('system.tokens.created'))
    void tokens.refresh()
  }

  const columns = $derived<Column<TokenInfo>[]>([
    { key: 'name', label: t('common.label.name'), sortable: true, value: (x) => x.name, truncate: true, width: '30%' },
    ...(showOwner
      ? [{ key: 'owner', label: t('system.tokens.owner'), cell: ownerCell, value: (x) => x.username, sortable: true } satisfies Column<TokenInfo>]
      : []),
    { key: 'scope', label: t('system.tokens.scopeLabel'), cell: scopeCell, value: (x) => x.scope, sortable: true },
    { key: 'prefix', label: t('system.tokens.prefix'), mono: true, value: (x) => x.prefix, format: (x) => `${x.prefix}…` },
    {
      key: 'lastUsed',
      label: t('system.tokens.lastUsed'),
      sortable: true,
      value: (x) => (x.lastUsed ? Date.parse(x.lastUsed) : undefined),
      format: (x) => (x.lastUsed ? formatRelative(x.lastUsed) : t('common.state.never')),
    },
    {
      key: 'expires',
      label: t('system.tokens.expires'),
      sortable: true,
      cell: expiresCell,
      value: (x) => (x.expiresAt ? Date.parse(x.expiresAt) : undefined),
    },
    {
      key: 'created',
      label: t('common.label.created'),
      sortable: true,
      value: (x) => Date.parse(x.createdAt),
      format: (x) => formatDate(x.createdAt),
    },
    { key: 'actions', label: t('common.label.actions'), align: 'right', cell: actionsCell, width: '64px' },
  ])
</script>

{#snippet ownerCell(x: TokenInfo)}
  <span class="owner">
    <span class="truncate">{x.username}</span>
    {#if mine(x)}<Badge tone="info">{t('system.users.you')}</Badge>{/if}
  </span>
{/snippet}

{#snippet scopeCell(x: TokenInfo)}
  {#if x.scope === 'admin'}
    <Badge tone="warn">{t('system.tokens.scope.admin')}</Badge>
  {:else}
    <Badge>{t('system.tokens.scope.read')}</Badge>
  {/if}
{/snippet}

{#snippet expiresCell(x: TokenInfo)}
  {#if expired(x)}
    <Badge tone="fail">{t('system.tokens.expired')}</Badge>
  {:else if x.expiresAt}
    <span title={formatDateTime(x.expiresAt)}>{formatDate(x.expiresAt)}</span>
  {:else}
    <span class="muted">{t('common.state.never')}</span>
  {/if}
{/snippet}

{#snippet actionsCell(x: TokenInfo)}
  <IconButton
    icon="trash"
    size="sm"
    variant="danger"
    label={t('system.tokens.deleteNamed', { name: x.name })}
    onclick={() => remove(x)}
  />
{/snippet}

<div class="page">
  <Panel title={t('system.tokens.title')} description={t('system.tokens.description')} flush>
    {#snippet actions()}
      <Button variant="primary" icon="plus" onclick={() => (createOpen = true)}>
        {t('system.tokens.create')}
      </Button>
    {/snippet}
    <Table
      {columns}
      rows={tokens.data}
      key={(x) => x.id}
      loading={tokens.loading}
      error={tokens.error && !tokens.data ? errorText(tokens.error) : undefined}
      onretry={() => tokens.refresh()}
      onrowclick={(x) => select(x)}
      selected={selectedId}
      caption={t('system.tokens.title')}
      skeletonRows={3}
    >
      {#snippet empty()}
        <EmptyState icon="key" title={t('system.tokens.emptyTitle')} text={t('system.tokens.emptyText')} compact>
          <Button icon="plus" onclick={() => (createOpen = true)}>
            {t('system.tokens.create')}
          </Button>
        </EmptyState>
      {/snippet}
    </Table>
  </Panel>

  <MetricsPanel />
</div>

{#if createOpen}
  <CreateTokenDialog bind:open={createOpen} oncreated={created} />
{/if}

<SidePanel
  open={!!selectedId}
  title={selected?.name ?? t('system.tokens.title')}
  subtitle={selected ? `${selected.prefix}…` : undefined}
  onclose={() => select(undefined)}
>
  {#if selected}
    <div class="stack">
      {#if expired(selected)}
        <Notice tone="warn">{t('system.tokens.expiredText')}</Notice>
      {/if}
      <KeyValue
        items={[
          ...(showOwner ? [{ label: t('system.tokens.owner'), value: selected.username }] : []),
          {
            label: t('system.tokens.scopeLabel'),
            value: selected.scope === 'admin' ? t('system.tokens.scope.admin') : t('system.tokens.scope.read'),
          },
          { label: t('system.tokens.prefix'), value: `${selected.prefix}…`, mono: true },
          { label: t('common.label.created'), value: formatDateTime(selected.createdAt) },
          {
            label: t('system.tokens.lastUsed'),
            value: selected.lastUsed ? formatDateTime(selected.lastUsed) : t('common.state.never'),
          },
          {
            label: t('system.tokens.expires'),
            value: selected.expiresAt ? formatDateTime(selected.expiresAt) : t('common.state.never'),
          },
        ]}
      />
      <p class="small muted">
        {selected.scope === 'admin' ? t('system.tokens.scopeHelp.admin') : t('system.tokens.scopeHelp.read')}
      </p>
    </div>
  {:else if tokens.loaded}
    <p class="small muted">{t('system.tokens.gone')}</p>
  {/if}
  {#snippet actions()}
    {#if selected}
      <Button variant="danger" icon="trash" onclick={() => remove(selected)}>
        {t('system.tokens.delete')}
      </Button>
    {/if}
  {/snippet}
</SidePanel>

<style>
  .owner {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    max-width: 100%;
    min-width: 0;
  }
</style>
