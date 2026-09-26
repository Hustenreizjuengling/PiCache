<!--
  @component
  HTTPS certificate of the web UI: the certificate in use and which of
  PiCache's names and addresses it covers, the local CA (download, trust
  guide, creating a new one) and uploading your own certificate. Without
  an HTTPS listener (PICACHE_WEB_TLS_LISTEN off) only a notice. When a
  change here replaces the certificate this page itself was loaded with,
  it says to trust the new one and reload: the open connection keeps the
  old certificate, and background requests over a new connection fail
  without the browser's certificate prompt.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type TlsStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Notice, Skeleton } from '$lib/ui'
  import CertificatePanel from './https/CertificatePanel.svelte'
  import LocalCaPanel from './https/LocalCaPanel.svelte'
  import TrustGuide from './https/TrustGuide.svelte'
  import UploadPanel from './https/UploadPanel.svelte'

  const tls = resource((signal) => api.tls.status({ signal }))
  const status = $derived(tls.data)

  /** This page came straight from PiCache's HTTPS port (not through a reverse proxy with its own certificate). */
  const direct = $derived(
    location.protocol === 'https:' && (location.port || '443') === String(session.status?.httpsPort ?? ''),
  )
  let certChanged = $state(false)

  /** Takes the status an action returned and notes when the served certificate changed. */
  function update(st: TlsStatus) {
    const before = tls.data?.certificate?.fingerprintSha256
    tls.set(st)
    if (direct && before && st.certificate?.fingerprintSha256 !== before) certChanged = true
  }
</script>

<div class="page">
  {#if !status}
    {#if tls.error}
      <Notice tone="fail" title={t('system.https.loadError')}>
        {errorText(tls.error)}
        {#snippet actions()}
          <Button size="sm" icon="refresh" onclick={() => tls.refresh()}>{t('common.action.retry')}</Button>
        {/snippet}
      </Notice>
    {:else}
      <Skeleton height="240px" />
      <Skeleton height="160px" />
    {/if}
  {:else if !status.listener}
    <Notice tone="info" title={t('system.https.noListener')}>{t('system.https.noListenerText')}</Notice>
  {:else}
    {#if certChanged}
      <Notice tone="warn" icon="lock" title={t('system.https.changedTitle')} ondismiss={() => (certChanged = false)}>
        {t('system.https.changedText')}
        {#snippet actions()}
          <Button size="sm" icon="refresh" onclick={() => location.reload()}>{t('common.action.reload')}</Button>
        {/snippet}
      </Notice>
    {/if}
    <CertificatePanel {status} loading={tls.loading} onrefresh={() => tls.refresh()} />
    <LocalCaPanel {status} onchange={update} />
    {#if status.localCa}<TrustGuide />{/if}
    <UploadPanel {status} onchange={update} />
  {/if}
</div>
