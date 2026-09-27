<!--
  @component
  Edits the saved listeners (PUT /system/listeners, applied at the next
  start): per service its default, off (where a service may be off) or its
  own addresses, one "ip:port" or ":port" per line. Services set by an
  environment variable or a command-line flag are shown fixed. Saving asks
  for the current password; the server checks the addresses (this
  machine's, no port shared by two services, a web listener kept on all
  addresses or loopback and on the address this browser is connected
  through) and names the service and line of a problem, also for a service
  set on the host. When the saved set would no longer serve the address this
  page is open at, it says where to open PiCache after the restart.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type ListenerRole, type ListenerRoleConfig, type ListenersConfig } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Dialog, Field, Icon, Input, Notice, Select, SidePanel, toast } from '$lib/ui'
  import { lineError } from '../../dns/shared/errors'
  import LinesInput from '../../dns/shared/LinesInput.svelte'
  import { draftAddresses, draftOf, pageMoves, ROLE_INFO, savedSet, webUrls, type RoleDraft, type RoleMode } from './listeners'

  interface Props {
    open?: boolean
    config: ListenersConfig
    onsaved: (config: ListenersConfig) => void
  }

  let { open = $bindable(false), config, onsaved }: Props = $props()

  let drafts = $state<Partial<Record<ListenerRole, RoleDraft>>>({})
  let err = $state.raw<ApiError | undefined>(undefined)

  // Every opening starts from the saved set.
  $effect.pre(() => {
    if (!open) return
    untrack(() => {
      drafts = Object.fromEntries(config.roles.map((r) => [r.role, draftOf(r)]))
      err = undefined
    })
  })

  /**
   * Whether the saved set may turn a role off: the server's answer, except
   * that the web listener follows the draft (off only while the saved set
   * gives the HTTPS listener its own addresses).
   */
  function canDisable(r: ListenerRoleConfig): boolean {
    if (r.role !== 'web') return r.canDisable
    const tls = drafts.webTls
    const locked = config.roles.some((x) => x.role === 'webTls' && x.locked)
    return !!tls && !locked && tls.mode === 'custom' && tls.addresses.length > 0
  }

  function modeOptions(r: ListenerRoleConfig) {
    const def = r.default.length > 0 ? r.default.join(', ') : t('system.health.listeners.off')
    return [
      { value: 'default', label: t('system.network.listeners.modeDefault', { addresses: def }) },
      // A role already off keeps the choice (the server names the problem when saving).
      ...(canDisable(r) || drafts[r.role]?.mode === 'off' ? [{ value: 'off', label: t('system.network.listeners.modeOff') }] : []),
      { value: 'custom', label: t('system.network.listeners.modeCustom') },
    ]
  }

  function setMode(r: ListenerRoleConfig, v: string) {
    const d = drafts[r.role]
    if (!d) return
    d.mode = (v === 'off' || v === 'custom' ? v : 'default') as RoleMode
    // Starting a list: begin with what the service uses now.
    if (d.mode === 'custom' && d.addresses.length === 0) d.addresses = [...(r.saved?.length ? r.saved : r.bound.length ? r.bound : r.default)]
  }

  function roleError(role: ListenerRole): string | undefined {
    return lineError(err, `listeners.${role}`)
  }

  const general = $derived(err && !err.field?.startsWith('listeners') && err.field !== 'currentPassword' ? errorText(err) : undefined)
  /** The draft no longer serves this page's address: where to open PiCache after the restart. */
  const moved = $derived.by(() => {
    const next = (r: ListenerRoleConfig) => draftAddresses(r, drafts[r.role])
    return pageMoves(config.roles, next) ? webUrls(config.roles, next) : undefined
  })
  /** A custom list left empty (the server would store it as off). */
  const emptyCustom = $derived(config.roles.filter((r) => !r.locked && drafts[r.role]?.mode === 'custom' && drafts[r.role]?.addresses.length === 0))

  // ---- confirmation with the current password
  let confirmOpen = $state(false)
  let password = $state('')
  let submitted = $state(false)
  let saving = $state(false)
  let passwordErr = $state.raw<ApiError | undefined>(undefined)

  const passwordError = $derived(
    fieldError(passwordErr, 'currentPassword') ?? (submitted && !password ? t('system.network.listeners.passwordRequired') : undefined),
  )
  const passwordGeneral = $derived(passwordErr && !passwordErr.field ? errorText(passwordErr) : undefined)

  function ask(e: SubmitEvent) {
    e.preventDefault()
    if (emptyCustom.length > 0) return
    password = ''
    submitted = false
    passwordErr = undefined
    confirmOpen = true
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    passwordErr = undefined
    if (!password) return
    saving = true
    try {
      const cfg = await api.system.saveListeners({ listeners: savedSet(config.roles, drafts), currentPassword: password })
      confirmOpen = false
      open = false
      toast.success(t('system.network.listeners.saved'))
      onsaved(cfg)
    } catch (ex) {
      const ae = toApiError(ex)
      if (ae.field === 'currentPassword' || ae.code === 'too_many_requests') {
        passwordErr = ae // stays in the dialog: try again
      } else {
        confirmOpen = false
        err = ae
      }
    } finally {
      saving = false
    }
  }

  function confirmClosed() {
    password = ''
    submitted = false
  }

  const auto = $props.id()
</script>

{#snippet movedNotice(urls: string[])}
  <Notice tone="warn" title={t('system.network.listeners.movedTitle')}>
    {urls.length > 0
      ? t('system.network.listeners.movedText', { current: location.origin, urls: urls.join(', ') })
      : t('system.network.listeners.movedTextNoUrl', { current: location.origin })}
  </Notice>
{/snippet}

<SidePanel bind:open title={t('system.network.listeners.editTitle')} size="lg" dismissible={!saving}>
  <form id="listeners-{auto}" class="stack" onsubmit={ask} novalidate>
    <p class="small muted">{t('system.network.listeners.editHelp')}</p>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}
    {#if moved}{@render movedNotice(moved)}{/if}
    {#each config.roles as r (r.role)}
      {@const d = drafts[r.role]}
      {@const info = ROLE_INFO[r.role]}
      {#if d && info}
        <fieldset class="role">
          <legend>{t(info.label)}</legend>
          <p class="env mono">{info.env}</p>
          {#if r.locked}
            <p class="small muted">
              {r.lockedBy === 'flag'
                ? t('system.network.listeners.lockedFlagEdit')
                : t('system.network.listeners.lockedEdit', { env: r.lockedBy ?? info.env })}
            </p>
            <p class="mono small">{r.bound.join(', ') || t('system.health.listeners.off')}</p>
            {#if roleError(r.role)}
              <!-- The other services' changes clash with it: named here, since it has no field. -->
              <p class="role-error" role="alert"><Icon name="alert" size={16} />{roleError(r.role)}</p>
            {/if}
          {:else}
            <Field label={t('system.network.listeners.mode', { service: t(info.label) })} hideLabel error={d.mode === 'custom' ? undefined : roleError(r.role)}>
              <Select value={d.mode} options={modeOptions(r)} onchange={(e) => setMode(r, e.currentTarget.value)} />
            </Field>
            {#if d.mode === 'custom'}
              <Field
                label={t('system.network.listeners.addresses')}
                help={t('system.network.listeners.addressesHelp')}
                error={roleError(r.role) ?? (emptyCustom.includes(r) ? t('system.network.listeners.addressesRequired') : undefined)}
              >
                <LinesInput bind:value={d.addresses} rows={Math.min(8, Math.max(2, d.addresses.length + 1))} placeholder={r.default[0] ?? info.example} />
              </Field>
            {/if}
          {/if}
        </fieldset>
      {/if}
    {/each}
  </form>

  {#snippet actions()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="listeners-{auto}" variant="primary" disabled={!session.isAdmin}>{t('common.action.save')}</Button>
  {/snippet}
</SidePanel>

<Dialog bind:open={confirmOpen} title={t('system.network.listeners.confirmTitle')} size="sm" dismissible={!saving} onclose={confirmClosed}>
  <form id="listeners-pw-{auto}" class="stack" onsubmit={save} novalidate>
    <p class="small muted">{t('system.network.listeners.confirmText')}</p>
    {#if moved}{@render movedNotice(moved)}{/if}
    {#if passwordGeneral}<Notice tone="fail">{passwordGeneral}</Notice>{/if}
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
    <Field label={t('system.network.listeners.password')} error={passwordError}>
      <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={saving} onclick={() => (confirmOpen = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="listeners-pw-{auto}" variant="primary" loading={saving}>{t('system.network.listeners.confirm')}</Button>
  {/snippet}
</Dialog>

<style>
  .role {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    margin: 0;
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  legend {
    padding: 0 var(--sp-1);
    font-weight: 600;
  }
  .env {
    margin-top: calc(-1 * var(--sp-1));
    color: var(--text-3);
    font-size: var(--fs-xs);
    overflow-wrap: anywhere;
  }
  .mono.small {
    overflow-wrap: anywhere;
  }
  .role-error {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    font-size: var(--fs-sm);
    color: var(--danger);
    overflow-wrap: anywhere;
  }
  .role-error :global(.icon) {
    flex: none;
    margin-top: 1px;
  }
</style>
