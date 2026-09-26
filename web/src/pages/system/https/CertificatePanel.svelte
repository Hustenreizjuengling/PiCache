<!--
  @component
  The certificate the HTTPS port serves: where it comes from, subject,
  issuer, validity with the days left, names and addresses, key type and
  SHA-256 fingerprint; a configured certificate that cannot be used (a
  fallback is served then) or a temporary certificate while PiCache cannot
  store its own; and which of PiCache's names and addresses it
  covers (browsers warn for the others).
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { TlsStatus } from '$lib/api'
  import { formatDate, formatDateTime, formatRelative } from '$lib/format'
  import { Badge, CopyButton, IconButton, KeyValue, Notice, Panel } from '$lib/ui'
  import { daysLeft, expiryTone, sentence, SOURCE_LABEL } from './cert'

  let { status, loading, onrefresh }: { status: TlsStatus; loading: boolean; onrefresh: () => void } = $props()

  const cert = $derived(status.certificate)
  /**
   * PiCache cannot store its own certificate (<data>/tls unusable) and
   * serves a temporary one: the only error that comes with the source
   * self-signed and no fallback.
   */
  const temporary = $derived(status.source === 'self-signed' && !!status.error && !status.fallback)
  const left =$derived(cert ? daysLeft(cert.notAfter) : 0)
  const validity = $derived(
    !cert
      ? ''
      : left < 0
        ? t('system.https.cert.expired', { date: formatDate(cert.notAfter) })
        : tn('system.https.cert.daysLeft', left, { date: formatDate(cert.notAfter) }),
  )
</script>

<Panel title={t('system.https.cert.title')} description={t('system.https.cert.description')}>
  {#snippet actions()}
    {#if status.checkedAt}
      <span class="small muted" title={formatDateTime(status.checkedAt)}>
        {t('system.https.cert.checkedAt', { time: formatRelative(status.checkedAt) })}
      </span>
    {/if}
    <IconButton icon="refresh" label={t('common.action.refresh')} {loading} onclick={onrefresh} />
  {/snippet}

  <div class="stack">
    {#if status.error && temporary}
      <Notice tone="warn" title={t('system.https.cert.tempTitle')}>
        <p>{sentence(status.error)}</p>
        <p>{t('system.https.cert.temp')}</p>
      </Notice>
    {:else if status.error}
      <Notice tone="fail" title={t('system.https.cert.errorTitle')}>
        <p>{sentence(status.error)}</p>
        {#if status.fallback}<p>{t('system.https.cert.fallback')}</p>{/if}
      </Notice>
    {/if}

    {#if cert}
      <div class="head">
        <Badge tone="info">{t(SOURCE_LABEL[status.source])}</Badge>
        <Badge tone={expiryTone(cert.notAfter)}>{validity}</Badge>
      </div>
      <KeyValue
        items={[
          { label: t('system.https.cert.subject'), value: cert.subject, mono: true },
          {
            label: t('system.https.cert.issuer'),
            value: cert.selfSigned ? t('system.https.cert.selfSigned') : cert.issuer,
            mono: !cert.selfSigned,
          },
          { label: t('system.https.cert.notBefore'), value: formatDateTime(cert.notBefore) },
          { label: t('system.https.cert.notAfter'), value: formatDateTime(cert.notAfter) },
          { label: t('system.https.cert.keyType'), value: cert.keyType === 'other' ? t('system.https.cert.keyOther') : cert.keyType },
          { label: t('system.https.cert.chain'), value: tn('system.https.cert.chainLength', cert.chainLength) },
        ]}
      >
        <dt>{t('system.https.cert.sans')}</dt>
        <dd>
          <ul class="names">
            {#each cert.sans as n (n)}<li class="mono">{n}</li>{/each}
          </ul>
        </dd>
        <dt>{t('system.https.cert.fingerprint')}</dt>
        <dd class="fp">
          <code class="mono">{cert.fingerprintSha256}</code>
          <CopyButton text={cert.fingerprintSha256} label={t('system.https.cert.copyFingerprint')} />
        </dd>
      </KeyValue>
    {/if}

    <div class="cover">
      <section class="stack-sm" aria-labelledby="https-covered">
        <h3 id="https-covered">{t('system.https.hosts.covered')}</h3>
        {#if status.hostsCovered.length > 0}
          <ul class="hosts">
            {#each status.hostsCovered as h (h)}<li class="mono ok">{h}</li>{/each}
          </ul>
        {:else}
          <p class="small subtle">{t('system.https.hosts.none')}</p>
        {/if}
      </section>
      <section class="stack-sm" aria-labelledby="https-not-covered">
        <h3 id="https-not-covered">{t('system.https.hosts.notCovered')}</h3>
        {#if status.hostsNotCovered.length > 0}
          <ul class="hosts">
            {#each status.hostsNotCovered as h (h)}<li class="mono warn">{h}</li>{/each}
          </ul>
          <p class="small muted">{t('system.https.hosts.warn')}</p>
        {:else}
          <p class="small subtle">{t('system.https.hosts.allCovered')}</p>
        {/if}
      </section>
    </div>
  </div>
</Panel>

<style>
  .head {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
  .names {
    display: flex;
    flex-wrap: wrap;
    gap: 2px var(--sp-3);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .fp {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-1);
  }
  .fp code {
    min-width: 0;
    overflow-wrap: anywhere;
  }
  h3 {
    font-size: var(--fs-md);
  }
  .cover {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  @media (max-width: 700px) {
    .cover {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .hosts {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .hosts li {
    padding: 1px 8px;
    border: 1px solid var(--line);
    border-radius: var(--r-pill);
    font-size: var(--fs-sm);
  }
  .hosts .ok {
    border-color: color-mix(in srgb, var(--ok) 45%, var(--line));
  }
  .hosts .warn {
    border-color: var(--warn);
    background: color-mix(in srgb, var(--warn) 10%, var(--surface));
  }
</style>
