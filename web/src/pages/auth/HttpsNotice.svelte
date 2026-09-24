<!--
  @component
  Warns on the sign-in and setup screens when the page was opened over plain
  HTTP while PiCache also listens on HTTPS (/auth/status httpsPort), and
  links to the same page on the HTTPS port.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { session } from '../../lib/session.svelte'
  import { Notice } from '../../lib/ui'
  import { httpsUrl } from './https'

  const url = $derived(httpsUrl(location.href, session.status?.httpsPort ?? 0))
</script>

{#if url}
  <Notice tone="warn" icon="lock" title={t('auth.https.title')}>
    <p>{t('auth.https.text')}</p>
    <p><a class="mono link" href={url}>{url}</a></p>
    <p class="small muted">{t('auth.https.cert')}</p>
  </Notice>
{/if}

<style>
  .link {
    overflow-wrap: anywhere;
  }
</style>
