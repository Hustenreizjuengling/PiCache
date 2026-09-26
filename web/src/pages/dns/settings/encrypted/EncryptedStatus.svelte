<!--
  @component
  The state of encrypted DNS (GET /dns/encrypted, the saved settings): per
  protocol Serving, Not serving (with the reason) or Off, the bound
  listeners, the DoT host (with the port when no listener is on 853) and
  the DoH addresses to copy; the certificate (source, usable, whether it
  covers the server name and the names below it) with a link to System >
  HTTPS certificate, and "Create a new local CA" when the local CA's leaf
  is served and the CA does not permit the server name; whether devices
  can discover the endpoints (DDR) or why not; the queries answered since
  the start.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, DdrReason, EncryptedDnsStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatNumber } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { Badge, Button, Chip, Icon, IconButton, KeyValue, Notice, Skeleton, type Tone } from '$lib/ui'
  import { sentence, SOURCE_LABEL } from '../../../system/https/cert'
  import CopyValue from '../../network/CopyValue.svelte'
  import { dotDevicePort, dotHost, DOT_PORT } from './endpoints'

  interface Props {
    status: EncryptedDnsStatus | undefined
    error: ApiError | undefined
    loading: boolean
    onrefresh: () => void
  }

  let { status, error, loading, onrefresh }: Props = $props()

  type State = 'serving' | 'notServing' | 'off'

  function stateOf(p: { enabled: boolean; serving: boolean }): State {
    return !p.enabled ? 'off' : p.serving ? 'serving' : 'notServing'
  }

  const TONE: Record<State, Tone> = { serving: 'ok', notServing: 'fail', off: 'neutral' }

  const DDR_REASON: Record<DdrReason, string> = $derived({
    off: t('dns.settings.encrypted.ddr.off'),
    'no-server-name': t('dns.settings.encrypted.ddr.noServerName'),
    'not-serving': t('dns.settings.encrypted.ddr.notServing'),
    'no-ip-address': t('dns.settings.encrypted.ddr.noIpAddress'),
  })

  const dotState = $derived(status ? stateOf(status.dot) : 'off')
  const dohState = $derived(status ? stateOf(status.doh) : 'off')
  const dotPort = $derived(status ? dotDevicePort(status.dot) : DOT_PORT)
  const host = $derived(status?.dot.host ? dotHost(status.dot.host, dotPort) : '')
  const cert = $derived(status?.certificate)
  const name = $derived(status?.serverName ?? '')
  /**
   * The local CA's leaf is served and misses the server name, and the CA needs
   * renewing: only a new CA helps (its name constraint permits the name and the
   * names below it together). localCaRenewalNeeded alone also reports other
   * names and addresses, and it stays set while a public certificate is served.
   */
  const caMissesName = $derived(
    !!cert && !!name && cert.source === 'local-ca' && cert.localCaRenewalNeeded && !cert.covered,
  )
</script>

{#snippet stateChip(s: State)}
  <Chip size="sm" tone={TONE[s]} label={t(`dns.settings.encrypted.state.${s}`)} />
{/snippet}

{#snippet coverage(tone: 'ok' | 'warn' | 'neutral', text: string)}
  <span class={['cov', tone]}>
    <Icon name={tone === 'ok' ? 'success' : tone === 'warn' ? 'alert' : 'info'} size={16} />
    <span>{text}</span>
  </span>
{/snippet}

<section class="stack-sm sub" aria-labelledby="dns-enc-status">
  <div class="head">
    <h3 id="dns-enc-status">{t('dns.settings.encrypted.status.title')}</h3>
    <IconButton icon="refresh" size="sm" label={t('common.action.refresh')} {loading} onclick={onrefresh} />
  </div>

  {#if !status}
    {#if error}
      <Notice tone="fail" title={t('dns.settings.encrypted.status.loadError')}>{errorText(error)}</Notice>
    {:else}
      <Skeleton height="120px" />
    {/if}
  {:else}
    <KeyValue>
      <dt>{t('dns.settings.encrypted.dotShort')}</dt>
      <dd class="lines">
        <span class="line">
          {@render stateChip(dotState)}
          {#if dotState === 'notServing' && status.dot.error}<span class="bad">{sentence(status.dot.error)}</span>{/if}
        </span>
        {#if host}
          <span class="line"><span class="muted">{t('dns.settings.encrypted.status.host')}</span><CopyValue value={host} /></span>
          {#if dotPort !== DOT_PORT}
            <span class="small warn">{t('dns.settings.encrypted.status.dotPort', { port: String(dotPort) })}</span>
          {/if}
        {/if}
        {#if status.dot.listeners.length > 0}
          <span class="small muted">{t('dns.settings.encrypted.status.listening')} <span class="mono">{status.dot.listeners.join(', ')}</span></span>
        {/if}
      </dd>

      <dt>{t('dns.settings.encrypted.dohShort')}</dt>
      <dd class="lines">
        <span class="line">
          {@render stateChip(dohState)}
          {#if dohState === 'notServing' && status.doh.error}<span class="bad">{sentence(status.doh.error)}</span>{/if}
        </span>
        {#each status.doh.urls as u (u)}
          <span class="line"><CopyValue value={u} /></span>
        {/each}
        {#if status.doh.listeners.length > 0}
          <span class="small muted">
            {t('dns.settings.encrypted.status.listening')}
            {#each status.doh.listeners as l, i (l.address)}{i > 0 ? ', ' : ''}<span class="mono">{l.address}</span
              >{l.role === 'web-tls' ? ` ${t('dns.settings.encrypted.status.webTls')}` : ''}{/each}
          </span>
        {/if}
      </dd>

      {#if cert}
        <dt>{t('dns.settings.encrypted.cert.title')}</dt>
        <dd class="lines">
          {#if cert.source === 'none'}
            <span class="muted">{t('dns.settings.encrypted.cert.none')}</span>
          {:else}
            <span class="line">
              <Badge tone="info">{t(SOURCE_LABEL[cert.source])}</Badge>
              {#if !cert.usable}
                <Chip size="sm" tone="fail" label={t('dns.settings.encrypted.cert.unusable')} />
              {/if}
            </span>
            {#if cert.error}<span class="bad">{sentence(cert.error)}</span>{/if}
            {#if name}
              <!-- Text lines, not chips: long server names must wrap on narrow screens. -->
              {@render coverage(
                cert.covered ? 'ok' : 'warn',
                t(cert.covered ? 'dns.settings.encrypted.cert.covered' : 'dns.settings.encrypted.cert.notCovered', { name }),
              )}
              {@render coverage(
                cert.wildcardCovered ? 'ok' : status.dot.enabled ? 'warn' : 'neutral',
                t(cert.wildcardCovered ? 'dns.settings.encrypted.cert.wildcard' : 'dns.settings.encrypted.cert.noWildcard', { name: `*.${name}` }),
              )}
            {/if}
          {/if}
          <a class="small" href={href('/system/https')}>{t('dns.settings.encrypted.cert.open')}</a>
        </dd>
      {/if}

      <dt>{t('dns.settings.encrypted.ddr.title')}</dt>
      <dd>
        {#if status.ddr.active}
          <span class="line">
            <Chip size="sm" tone="ok" label={t('dns.settings.encrypted.ddr.active')} />
            <span class="muted">{t('dns.settings.encrypted.ddr.activeHelp')}</span>
          </span>
        {:else}
          <span class="muted">{status.ddr.reason ? DDR_REASON[status.ddr.reason] : '–'}</span>
        {/if}
      </dd>

      <dt>{t('dns.settings.encrypted.status.queries')}</dt>
      <dd>
        {t('dns.settings.encrypted.status.queryCounts', {
          dot: formatNumber(status.queries.dot),
          doh: formatNumber(status.queries.doh),
        })}
      </dd>
    </KeyValue>

    {#if caMissesName}
      <Notice tone="warn" title={t('dns.settings.encrypted.cert.renewTitle')}>
        {t('dns.settings.encrypted.cert.renewText', { name })}
        {#snippet actions()}
          <Button size="sm" icon="lock" href={href('/system/https')}>{t('dns.settings.encrypted.cert.renew')}</Button>
        {/snippet}
      </Notice>
    {/if}
  {/if}
</section>

<style>
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .lines {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--sp-1);
  }
  .line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    max-width: 100%;
  }
  .bad {
    color: var(--danger);
  }
  .cov {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .cov > :global(.icon) {
    flex: none;
    margin-top: 2px;
  }
  .cov.ok > :global(.icon) {
    color: var(--ok);
  }
  .cov.warn > :global(.icon) {
    color: var(--warning);
  }
  .cov.neutral > :global(.icon) {
    color: var(--text-3);
  }
  .warn {
    color: var(--warning);
  }
</style>
