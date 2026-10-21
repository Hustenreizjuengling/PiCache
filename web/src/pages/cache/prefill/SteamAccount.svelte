<!--
  @component
  The Steam sign-in of the prefill service: a QR code that the Steam app
  scans and confirms (no password anywhere), the signed-in account with the
  token's expiry and the name under which it appears in Steam, signing out
  (revoked at Steam and deleted). The QR code follows the challenge URL,
  which Steam changes while the sign-in waits.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type PrefillStatus } from '$lib/api'
  import { formatDate, formatDateTime } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, KeyValue, Notice, Panel, Spinner, confirm, toast } from '$lib/ui'
  import QrCode from '$lib/ui/QrCode.svelte'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  const account = $derived(status.steam.account)
  const login = $derived(status.steam.login)
  const waiting = $derived(login?.state === 'waiting')
  const ended = $derived(!account && login && (login.state === 'failed' || login.state === 'expired'))

  let busy = $state(false)

  async function signIn() {
    busy = true
    try {
      await api.prefill.steamLogin()
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      busy = false
    }
  }

  async function cancelSignIn() {
    try {
      await api.prefill.steamCancelLogin()
      onchanged()
    } catch (err) {
      toast.error(err)
    }
  }

  async function signOut() {
    const ok = await confirm({
      title: t('cache.prefill.steam.signOutTitle'),
      message: t('cache.prefill.steam.signOutText'),
      confirmLabel: t('cache.prefill.steam.signOut'),
      action: () => api.prefill.steamLogout(),
    })
    if (ok) {
      toast.success(t('cache.prefill.steam.signedOut'))
      onchanged()
    }
  }
</script>

<Panel title={t('cache.prefill.steam.title')} description={t('cache.prefill.steam.description')}>
  {#snippet actions()}
    {#if account}
      {#if account.error}
        <Button icon="refresh" variant="primary" loading={busy} disabled={!session.canOperate || waiting} onclick={signIn}>
          {t('cache.prefill.steam.signInAgain')}
        </Button>
      {/if}
      <Button icon="logout" variant="danger" disabled={!session.canOperate} onclick={signOut}>{t('cache.prefill.steam.signOut')}</Button>
    {:else if !waiting}
      <Button icon="key" variant="primary" loading={busy} disabled={!session.canOperate} onclick={signIn}>{t('cache.prefill.steam.signIn')}</Button>
    {/if}
  {/snippet}
  <div class="stack">
    {#if waiting && login}
      <div class="qr">
        <QrCode text={login.challengeUrl} label={t('cache.prefill.steam.qrLabel')} fallback={t('cache.prefill.steam.qrFallback')} />
        <div class="steps">
          {#if login.scanned}
            <p class="row strong" role="status"><Spinner size={18} />{t('cache.prefill.steam.scanned')}</p>
          {:else}
            <ol>
              <li>{t('cache.prefill.steam.step1')}</li>
              <li>{t('cache.prefill.steam.step2')}</li>
              <li>{t('cache.prefill.steam.step3', { device: status.steam.deviceName })}</li>
            </ol>
          {/if}
          <p class="muted small">{t('cache.prefill.steam.qrHint')}</p>
          <div><Button size="sm" variant="ghost" onclick={cancelSignIn}>{t('cache.prefill.steam.cancel')}</Button></div>
        </div>
      </div>
    {:else if ended && login}
      <Notice tone="warn" title={login.state === 'expired' ? t('cache.prefill.steam.expired') : t('cache.prefill.steam.failed')}>
        {login.error}
      </Notice>
    {/if}

    {#if account}
      {#if account.error}
        <Notice tone="fail" title={t('cache.prefill.steam.invalid')}>{account.error}</Notice>
      {/if}
      <KeyValue
        items={[
          { label: t('cache.prefill.steam.account'), value: account.name },
          { label: t('cache.prefill.steam.since'), value: formatDateTime(account.since) },
          { label: t('cache.prefill.steam.expires'), value: account.expires ? formatDate(account.expires) : '–' },
          { label: t('cache.prefill.steam.device'), value: account.deviceName },
        ]}
      />
      <p class="muted small">{t('cache.prefill.steam.revokeHint')}</p>
    {:else if !waiting}
      <ul class="facts">
        <li>{t('cache.prefill.steam.fact1')}</li>
        <li>{t('cache.prefill.steam.fact2')}</li>
        <li>{t('cache.prefill.steam.fact3')}</li>
      </ul>
    {/if}
  </div>
</Panel>

<style>
  .qr {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-5);
    align-items: flex-start;
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    flex: 1 1 240px;
  }
  ol,
  .facts {
    margin: 0;
    padding-left: var(--sp-5);
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .facts {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .strong {
    font-weight: 600;
    gap: var(--sp-2);
  }
</style>
