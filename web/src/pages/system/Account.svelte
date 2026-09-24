<!--
  @component
  Account & security: password, two-factor authentication, signed-in
  sessions and the web interface settings (session lifetime, host names,
  HTTPS redirect, default language).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Notice } from '$lib/ui'
  import PasswordPanel from './account/PasswordPanel.svelte'
  import SessionsPanel from './account/SessionsPanel.svelte'
  import TotpPanel from './account/TotpPanel.svelte'
  import WebSettingsPanel from './account/WebSettingsPanel.svelte'

  const me = resource((signal) => api.auth.me({ signal }))
  let sessionsReload = $state(0)
</script>

<div class="page">
  {#if !session.isAdmin}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  <p class="muted intro">
    {t('system.account.signedInAs', { name: me.data?.username ?? session.user?.username ?? '–' })}
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

  <WebSettingsPanel />
</div>

<style>
  .intro {
    margin-bottom: calc(-1 * var(--sp-2));
  }
  .cols-2 {
    align-items: start;
  }
</style>
