<!--
  @component
  Setting up devices for encrypted DNS (from the saved state): per platform
  (collapsible) where to enter the DoT host or the DoH address, each with a
  copy button, and PiCache's address where a platform needs it (the one
  PiCache answers for its own names; the DoT port 853 while a listener is
  bound there); an optional ClientID (checked as typed) that turns them into
  <ClientID>.<server name> and /dns-query/<ClientID>, with "Add a client
  with this ClientID" for admins; Apple configuration profiles below
  (./AppleProfile.svelte, not for viewers). Nothing to set up until DoT or
  DoH is on with a server name.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import { api, resource, type ClientGroup, type EncryptedDnsStatus } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Button, CopyButton, Field, Input, Notice, Panel } from '$lib/ui'
  import ClientPanel from '../../clients/ClientPanel.svelte'
  import CopyValue from '../../network/CopyValue.svelte'
  import { CLIENT_ID_PREFIX, isClientId, normalizeClientId } from '../../shared/clientid'
  import AppleProfile from './AppleProfile.svelte'
  import { dotDevicePort, DOT_PORT, withClientIdHost, withClientIdUrl } from './endpoints'

  interface Props {
    status: EncryptedDnsStatus | undefined
    groups: readonly ClientGroup[] | undefined
    /** dns.serverNameAddresses.ipv4 (saved): PiCache's address for devices, when set. */
    serverNameAddresses: readonly string[]
    /**
     * downloadCache.cacheIpv4 (saved): in a container bridge network PiCache
     * answers its own names with these (the host's LAN address), not with the
     * container's interface address.
     */
    cacheIpv4: readonly string[]
  }

  let { status, groups, serverNameAddresses, cacheIpv4 }: Props = $props()

  const PLATFORMS = ['android', 'apple', 'windows', 'firefox', 'chrome', 'linux'] as const
  type Platform = (typeof PLATFORMS)[number]
  /** The protocol each platform uses (Apple devices take either through the profile). */
  const NEEDS: Record<Platform, 'dot' | 'doh' | undefined> = {
    android: 'dot',
    apple: undefined,
    windows: 'doh',
    firefox: 'doh',
    chrome: 'doh',
    linux: 'dot',
  }

  // The IPv4 address Windows and systemd-resolved send the queries to, as
  // PiCache answers its own names: dns.serverNameAddresses, else the cache
  // addresses (a container bridge network), else this machine's interfaces.
  const interfaces = resource((signal) => api.dhcp.interfaces({ signal }))
  const address = $derived(
    serverNameAddresses[0] ??
      cacheIpv4[0] ??
      (interfaces.data ?? []).filter((i) => !i.virtual).flatMap((i) => i.ipv4.map((a) => a.split('/')[0]))[0] ??
      '',
  )

  let clientIdText = $state('')
  const clientId = $derived(normalizeClientId(clientIdText))
  const clientIdValid = $derived(clientId !== '' && isClientId(clientId))
  const clientIdError = $derived(clientId && !clientIdValid ? t('dns.settings.encrypted.clientIdInvalid') : undefined)
  /** The ClientID applied to the shown values (only a valid one). */
  const id = $derived(clientIdValid ? clientId : '')

  const ready = $derived(!!status?.serverName && (status.dot.enabled || status.doh.enabled))
  const host = $derived(status?.dot.host ? withClientIdHost(status.dot.host, id) : '')
  const dohUrl = $derived(status?.doh.urls[0] ? withClientIdUrl(status.doh.urls[0], id) : '')
  const dotPort = $derived(status ? dotDevicePort(status.dot) : DOT_PORT)
  const resolved = $derived(
    `[Resolve]\nDNS=${address || '192.168.1.2'}${dotPort !== DOT_PORT ? `:${dotPort}` : ''}#${host}\nDNSOverTLS=yes`,
  )

  function enabled(p: 'dot' | 'doh'): boolean {
    return !!status?.[p].enabled
  }

  let addOpen = $state(false)
</script>

<Panel id="dns-set-devices" title={t('dns.settings.encrypted.setup.title')} description={t('dns.settings.encrypted.setup.description')}>
  <div class="stack">
    {#if status && !ready}
      <p class="small muted">{t('dns.settings.encrypted.setup.notReady')}</p>
    {:else if status}
      <div class="clientid">
        <Field
          label={t('dns.settings.encrypted.clientId')}
          optional
          help={t('dns.settings.encrypted.clientIdHelp')}
          error={clientIdError}
        >
          <Input bind:value={clientIdText} mono maxlength={63} placeholder="tims-ipad" autocomplete="off" autocapitalize="off" spellcheck={false} />
        </Field>
        {#if id}
          <p class="small muted">{t('dns.settings.encrypted.clientIdHint')}</p>
          {#if session.isAdmin}
            <div>
              <Button size="sm" icon="plus" onclick={() => (addOpen = true)}>{t('dns.settings.encrypted.addClient')}</Button>
            </div>
          {/if}
          {#if status.dot.enabled && !status.certificate.wildcardCovered}
            <Notice tone="warn">{t('dns.settings.encrypted.clientIdNoWildcard', { name: `*.${status.serverName}` })}</Notice>
          {/if}
        {/if}
      </div>

      <div class="platforms">
        {#each PLATFORMS as p (p)}
          {@const need = NEEDS[p]}
          <details>
            <summary>{t(`dns.settings.encrypted.setup.${p}.title` as MessageKey)}</summary>
            <div class="body small">
              {#if need && !enabled(need)}
                <p class="muted">{t(need === 'dot' ? 'dns.settings.encrypted.setup.needsDot' : 'dns.settings.encrypted.setup.needsDoh')}</p>
              {:else if p === 'apple'}
                <p class="muted">{t(session.canOperate ? 'dns.settings.encrypted.setup.apple.text' : 'dns.settings.encrypted.setup.apple.viewer')}</p>
              {:else if p === 'linux'}
                <p class="muted">{t('dns.settings.encrypted.setup.linux.text')}</p>
                <div class="code">
                  <pre class="mono">{resolved}</pre>
                  <CopyButton text={resolved} />
                </div>
                {#if !address}<p class="muted">{t('dns.settings.encrypted.setup.addressHint')}</p>{/if}
              {:else}
                <p class="muted">
                  {t(`dns.settings.encrypted.setup.${p}.text` as MessageKey, { address: address || t('dns.settings.encrypted.setup.yourAddress') })}
                </p>
                <p>
                  <CopyValue
                    value={p === 'android' ? host : dohUrl}
                    missing={t(
                      p !== 'android' && status.serverName
                        ? 'dns.settings.encrypted.setup.noDohListener'
                        : 'dns.settings.encrypted.setup.noAddress',
                    )}
                  />
                </p>
                {#if p === 'android' && dotPort !== DOT_PORT}
                  <p class="warn">{t('dns.settings.encrypted.setup.android.port')}</p>
                {/if}
              {/if}
              {#if need && enabled(need) && !status[need].serving}
                <p class="warn">{t(need === 'dot' ? 'dns.settings.encrypted.setup.dotNotServing' : 'dns.settings.encrypted.setup.dohNotServing')}</p>
              {/if}
            </div>
          </details>
        {/each}
      </div>
    {/if}

    {#if session.canOperate && status}
      <AppleProfile {status} />
    {/if}
  </div>
</Panel>

{#if session.isAdmin && id}
  <ClientPanel bind:open={addOpen} preset={{ name: id, identifiers: [CLIENT_ID_PREFIX + id] }} {groups} range="24h" />
{/if}

<style>
  .clientid {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    max-width: 480px;
  }
  .platforms {
    display: flex;
    flex-direction: column;
  }
  details {
    border-top: 1px solid var(--line);
  }
  details:last-child {
    border-bottom: 1px solid var(--line);
  }
  summary {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) 0;
    font-size: var(--fs-sm);
    font-weight: 600;
    list-style: none;
    cursor: pointer;
  }
  summary::-webkit-details-marker {
    display: none;
  }
  summary::before {
    content: '';
    flex: none;
    width: 6px;
    height: 6px;
    margin: 0 4px 0 2px;
    border-right: 1.5px solid var(--text-2);
    border-bottom: 1.5px solid var(--text-2);
    transform: rotate(-45deg);
    transition: transform var(--dur-fast);
  }
  details[open] > summary::before {
    transform: rotate(45deg);
  }
  summary:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: 0 0 var(--sp-3) var(--sp-5);
    max-width: 80ch;
    min-width: 0;
  }
  .code {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-1);
    min-width: 0;
  }
  pre {
    flex: 1 1 auto;
    min-width: 0;
    margin: 0;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .warn {
    color: var(--warning);
  }
</style>
