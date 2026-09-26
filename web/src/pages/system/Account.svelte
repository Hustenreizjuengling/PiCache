<!--
  @component
  Users & security: your own account (password, two-factor authentication,
  signed-in sessions; every session may change its own), the accounts and
  their roles (admins), which networks may use the web UI (web access) and
  the web interface settings (session lifetime, host names, HTTPS redirect,
  default language).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Badge, Notice } from '$lib/ui'
  import PasswordPanel from './account/PasswordPanel.svelte'
  import SessionsPanel from './account/SessionsPanel.svelte'
  import TotpPanel from './account/TotpPanel.svelte'
  import UsersPanel from './account/UsersPanel.svelte'
  import WebAccessPanel from './account/WebAccessPanel.svelte'
  import WebSettingsPanel from './account/WebSettingsPanel.svelte'

  const me = resource((signal) => api.auth.me({ signal }))
  let sessionsReload = $state(0)
  const user = $derived(me.data ?? session.user)
</script>

<div class="page">
  <p class="muted intro">
    <span>{t('system.account.signedInAs', { name: user?.username ?? '–' })}</span>
    {#if user}
      <Badge tone={user.role === 'admin' ? 'warn' : 'neutral'}>{t(`common.account.role.${user.role}`)}</Badge>
    {/if}
  </p>

  <div class="cols-2">
    <PasswordPanel onchanged={() => sessionsReload++} />
    <!-- Turning TOTP on signs out the other sessions: reload that list too. -->
    <TotpPanel
      user={me.data ?? session.user}
      onchange={() => {
        me.refresh()
        sessionsReload++
      }}
    />
  </div>

  <SessionsPanel reload={sessionsReload} />

  {#if session.canOperate}
    <UsersPanel onchanged={() => me.refresh()} />
  {:else}
    <Notice tone="info">{t('system.account.readOnlyBelow')}</Notice>
  {/if}

  <WebAccessPanel />

  <WebSettingsPanel />
</div>

<style>
  .intro {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    margin-bottom: calc(-1 * var(--sp-2));
  }
  .cols-2 {
    align-items: start;
  }
</style>
