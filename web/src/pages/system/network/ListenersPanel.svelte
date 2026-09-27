<!--
  @component
  The listeners by service (GET /system/listeners): the addresses in use
  now, what the next start uses (the saved set, the environment's value or
  the default) and the default. Listeners are read at start: a saved change
  waits for a restart ("Restart PiCache"), and a saved set that could not
  be bound at the last start is reported with its errors (PiCache used the
  environment or default value for those services). Admins change the
  saved set in the editor (confirmed with their password); services set by
  an environment variable or a command-line flag are marked and fixed. In
  Docker the compose file sets them: read-only, with the bridge-ports hint.
  When the next start no longer serves the address this page is open at,
  the restart notice and its confirmation name the new addresses; after a
  restart the panel reloads (onrestarted: the page's other panels).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ListenerRole, ListenerRoleConfig, ListenersConfig, Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Notice, Panel, Table, type Column } from '$lib/ui'
  import RestartButton from '../RestartButton.svelte'
  import ListenersEditor from './ListenersEditor.svelte'
  import { nextAddresses, pageMoves, ROLE_INFO, sameAddresses, webUrls } from './listeners'

  let { config, onrestarted }: { config: Resource<ListenersConfig>; onrestarted?: () => void } = $props()

  let editOpen = $state(false)

  const cfg = $derived(config.data)
  /** The roles of the last saved set that could not be bound, with their error and addresses. */
  const failedRoles = $derived.by(() => {
    const f = cfg?.failed
    if (!f) return []
    return (Object.keys(f.roles) as ListenerRole[])
      .filter((role) => role in ROLE_INFO)
      .map((role) => ({ role, error: f.roles[role] ?? '', saved: f.saved[role] ?? [] }))
  })

  /** The web interface's addresses at the next start, and whether this page's address is among them. */
  const nextUrls = $derived(cfg ? webUrls(cfg.roles, nextAddresses) : [])
  const moves = $derived(!!cfg && pageMoves(cfg.roles, nextAddresses))
  const movedText = $derived(
    nextUrls.length > 0
      ? t('system.network.listeners.movedText', { current: location.origin, urls: nextUrls.join(', ') })
      : t('system.network.listeners.movedTextNoUrl', { current: location.origin }),
  )

  function restarted() {
    void config.refresh()
    onrestarted?.()
  }

  function lockedText(r: ListenerRoleConfig): string {
    return r.lockedBy === 'flag' ? t('system.network.listeners.lockedFlag') : t('system.network.listeners.lockedBy', { env: r.lockedBy ?? '' })
  }

  const columns = $derived<Column<ListenerRoleConfig>[]>([
    { key: 'role', label: t('system.health.listeners.role'), cell: roleCell },
    { key: 'now', label: t('system.network.listeners.now'), cell: nowCell },
    { key: 'next', label: t('system.network.listeners.next'), cell: nextCell },
    { key: 'default', label: t('system.network.listeners.default'), cell: defaultCell },
  ])

  function saved(cfg: ListenersConfig) {
    config.set(cfg)
  }
</script>

{#snippet addresses(list: readonly string[])}
  {#if list.length > 0}
    <span class="addrs mono">
      {#each list as a (a)}<span>{a}</span>{/each}
    </span>
  {:else}
    <span class="subtle">{t('system.health.listeners.off')}</span>
  {/if}
{/snippet}

{#snippet roleCell(r: ListenerRoleConfig)}
  <span class="role">
    <span>{t(ROLE_INFO[r.role].label)}</span>
    <span class="env mono">{ROLE_INFO[r.role].env}</span>
  </span>
{/snippet}

{#snippet nowCell(r: ListenerRoleConfig)}
  {@render addresses(r.bound)}
{/snippet}

{#snippet nextCell(r: ListenerRoleConfig)}
  {@const next = nextAddresses(r)}
  <span class="next">
    {@render addresses(next)}
    {#if r.locked}
      <Badge tone="info" title={t('system.network.listeners.lockedHelp')}>{lockedText(r)}</Badge>
    {:else if r.saved === undefined}
      <span class="small muted">{t('system.network.listeners.isDefault')}</span>
    {/if}
    {#if !r.locked && !sameAddresses(next, r.bound)}
      <Badge tone="warn">{t('system.network.listeners.afterRestart')}</Badge>
    {/if}
  </span>
{/snippet}

{#snippet defaultCell(r: ListenerRoleConfig)}
  {@render addresses(r.default)}
{/snippet}

<Panel id="net-listeners" flush title={t('system.network.listeners.title')} description={t('system.network.listeners.description')}>
  {#snippet actions()}
    {#if cfg?.editable && session.isAdmin}
      <Button icon="edit" onclick={() => (editOpen = true)}>{t('system.network.listeners.edit')}</Button>
    {/if}
  {/snippet}

  {#if cfg && (cfg.reason === 'docker' || cfg.restartRequired || cfg.failed)}
    <div class="notes">
      {#if cfg.reason === 'docker'}
        <Notice tone="info" title={t('system.network.listeners.dockerTitle')}>{t('system.network.listeners.dockerText')}</Notice>
      {/if}
      {#if cfg.failed}
        <Notice tone="fail" title={t('system.network.listeners.failedTitle', { time: formatDateTime(cfg.failed.time) })}>
          <ul class="failed">
            {#each failedRoles as f (f.role)}
              <li>
                <strong>{t(ROLE_INFO[f.role].label)}</strong>:
                <span class="mono">{f.saved.join(', ') || '–'}</span>
                <span class="err">{f.error}</span>
              </li>
            {/each}
          </ul>
          <p>{t('system.network.listeners.failedText')}</p>
        </Notice>
      {/if}
      {#if cfg.restartRequired}
        <Notice tone="warn" title={t('system.network.listeners.restartTitle')}>
          {t('system.network.listeners.restartText')}
          {#if moves}<p class="moved">{movedText}</p>{/if}
          {#snippet actions()}
            <RestartButton
              variant="primary"
              size="sm"
              message={moves ? `${t('system.restart.confirmText')} ${movedText}` : undefined}
              timeoutText={nextUrls.length > 0 ? t('system.network.listeners.restartTimeout', { urls: nextUrls.join(', ') }) : undefined}
              ondone={restarted}
            />
          {/snippet}
        </Notice>
      {/if}
    </div>
  {/if}

  <Table
    {columns}
    rows={cfg?.roles}
    key={(r) => r.role}
    loading={config.loading && !config.loaded}
    error={config.error && !cfg ? errorText(config.error) : undefined}
    onretry={() => config.refresh()}
    caption={t('system.network.listeners.title')}
    skeletonRows={8}
  />
</Panel>

{#if cfg?.editable && session.isAdmin}
  <ListenersEditor bind:open={editOpen} config={cfg} onsaved={saved} />
{/if}

<style>
  .moved {
    margin-top: var(--sp-2);
    overflow-wrap: anywhere;
  }
  .notes {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    padding: 0 var(--sp-4) var(--sp-3);
  }
  /* Kept on one line: on narrow screens the table scrolls sideways instead. */
  .role {
    display: flex;
    flex-direction: column;
    padding: 4px 0;
    line-height: 1.3;
    white-space: nowrap;
  }
  .env {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .addrs {
    display: flex;
    flex-direction: column;
    line-height: 1.35;
  }
  .addrs > span {
    white-space: nowrap;
  }
  .next {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    padding: 4px 0;
  }
  .failed {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0 0 var(--sp-2);
    padding-left: var(--sp-5);
  }
  .failed li {
    overflow-wrap: anywhere;
  }
  .err {
    display: block;
    font-size: var(--fs-sm);
  }
</style>
