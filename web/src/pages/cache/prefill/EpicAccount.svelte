<!--
  @component
  The Epic sign-in of the prefill service: a device code. The QR code opens
  Epic's activation page on the phone, where the user signs in at Epic and
  confirms the code shown here; nothing is typed into PiCache. The signed-in
  account, signing out (ended at Epic and deleted).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type PrefillStatus } from '$lib/api'
  import { formatDateTime } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, KeyValue, Notice, Panel, Spinner, confirm, toast } from '$lib/ui'
  import QrCode from '$lib/ui/QrCode.svelte'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  const account = $derived(status.epic.account)
  const login = $derived(status.epic.login)
  const waiting = $derived(login?.state === 'waiting')
  const ended = $derived(!account && login && (login.state === 'failed' || login.state === 'expired'))

  let busy = $state(false)
  async function signIn() {
    busy = true
    try {
      await api.prefill.epicLogin()
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      busy = false
    }
  }

  async function cancelSignIn() {
    try {
      await api.prefill.epicCancelLogin()
      onchanged()
    } catch (err) {
      toast.error(err)
    }
  }

  async function signOut() {
    const ok = await confirm({
      title: t('cache.prefill.epic.signOutTitle'),
      message: t('cache.prefill.epic.signOutText'),
      confirmLabel: t('cache.prefill.epic.signOut'),
      action: () => api.prefill.epicLogout(),
    })
    if (ok) {
      toast.success(t('cache.prefill.epic.signedOut'))
      onchanged()
    }
  }
</script>

<Panel title={t('cache.prefill.epic.title')} description={t('cache.prefill.epic.description')}>
  {#snippet actions()}
    {#if account}
      {#if account.error}
        <Button icon="refresh" variant="primary" loading={busy} disabled={!session.canOperate || waiting} onclick={signIn}>
          {t('cache.prefill.epic.signInAgain')}
        </Button>
      {/if}
      <Button icon="logout" variant="danger" disabled={!session.canOperate} onclick={signOut}>{t('cache.prefill.epic.signOut')}</Button>
    {:else if !waiting}
      <Button icon="key" variant="primary" loading={busy} disabled={!session.canOperate} onclick={signIn}>{t('cache.prefill.epic.signIn')}</Button>
    {/if}
  {/snippet}
  <div class="stack">
    {#if waiting && login}
      <div class="qr">
        <QrCode text={login.verificationUrl} label={t('cache.prefill.epic.qrLabel')} fallback={login.verificationUrl} />
        <div class="steps">
          <ol>
            <li>{t('cache.prefill.epic.step1')}</li>
            <li>{t('cache.prefill.epic.step2')}</li>
            <li>{t('cache.prefill.epic.step3')} <span class="code mono">{login.userCode}</span></li>
          </ol>
          <p class="muted small">{t('cache.prefill.epic.link')} <a href={login.verificationUrl} target="_blank" rel="noreferrer noopener">{login.verificationUrl}</a></p>
          <p class="row muted small"><Spinner size={14} />{t('cache.prefill.epic.waiting')}</p>
          <div><Button size="sm" variant="ghost" onclick={cancelSignIn}>{t('cache.prefill.steam.cancel')}</Button></div>
        </div>
      </div>
    {:else if ended && login}
      <Notice tone="warn" title={login.state === 'expired' ? t('cache.prefill.epic.expired') : t('cache.prefill.epic.failed')}>{login.error}</Notice>
    {/if}

    {#if account}
      {#if account.error}
        <Notice tone="fail" title={t('cache.prefill.epic.invalid')}>{account.error}</Notice>
      {/if}
      <KeyValue
        items={[
          { label: t('cache.prefill.steam.account'), value: account.name },
          { label: t('cache.prefill.steam.since'), value: formatDateTime(account.since) },
        ]}
      />
      <p class="muted small">{t('cache.prefill.epic.revokeHint')}</p>
    {:else if !waiting}
      <ul class="facts">
        <li>{t('cache.prefill.epic.fact1')}</li>
        <li>{t('cache.prefill.epic.fact2')}</li>
        <li>{t('cache.prefill.epic.fact3')}</li>
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
  .code {
    font-weight: 700;
    font-size: var(--fs-lg);
    letter-spacing: 0.08em;
  }
  .row {
    gap: var(--sp-2);
  }
</style>
