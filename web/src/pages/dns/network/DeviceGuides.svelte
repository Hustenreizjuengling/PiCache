<!--
  @component
  DNS › Network check › Set up a device (#/dns/network/devices): where to
  enter PiCache as the DNS server on each kind of device, with this
  PiCache's values filled in and copyable: its IPv4 addresses and ULAs
  (/network/check, those of the default route's interface first; never
  global IPv6 addresses, which change with the provider's prefix, and none
  in a container bridge network, where they are the container's) and,
  while DoT or DoH serves, the DoT host name and the DoH address
  (/dns/encrypted; otherwise why not). The CA trust steps and the
  encrypted DNS set-up are linked, not repeated (Android's Private DNS
  needs a publicly trusted certificate instead). docs/DEVICES.md mirrors
  this page.
  Query: ?os=windows|macos|ios|android|linux|chromeos|consoles|tv
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import { api, resource, type NetworkCheck, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, CopyButton, Icon, Notice, Panel, Select, Skeleton, Tabs, Trans } from '$lib/ui'
  import { REPO } from '../../system/health/about'
  import CopyValue from './CopyValue.svelte'

  // The network check of the page (polled there): this machine's addresses.
  let { check }: { check: Resource<NetworkCheck> } = $props()

  const encrypted = resource((signal) => api.dns.encrypted({ signal }))

  const PLATFORMS = ['windows', 'macos', 'ios', 'android', 'linux', 'chromeos', 'consoles', 'tv'] as const
  type Platform = (typeof PLATFORMS)[number]

  const os = $derived.by((): Platform => {
    const v = router.param('os') as Platform
    return PLATFORMS.includes(v) ? v : 'windows'
  })

  function pick(id: string) {
    router.setQuery({ os: id === 'windows' ? null : id })
  }

  const tabs = $derived(PLATFORMS.map((p) => ({ id: p, label: t(`dns.devices.tab.${p}`) })))

  const settled = $derived((check.loaded || !!check.error) && (encrypted.loaded || !!encrypted.error))
  // In a container bridge network the addresses are the container's, which
  // devices cannot reach: the steps name what to enter instead.
  const bridge = $derived(check.data?.mode === 'bridge')
  const self = $derived(bridge ? undefined : check.data?.self)
  const ipv4 = $derived(self?.ipv4 ?? [])
  const ula = $derived(self?.ula ?? [])
  const enc = $derived(encrypted.data)
  // Only while the protocol serves: a device set to a name or address that
  // does not answer (strict mode) loses name resolution.
  const dotHost = $derived(enc?.dot.serving ? enc.dot.host : undefined)
  const dohUrl = $derived(enc?.doh.serving ? enc.doh.urls[0] : undefined)
  // Android rejects these for Private DNS (it trusts no CA a user installs).
  const untrustedCert = $derived(enc?.certificate.source === 'local-ca' || enc?.certificate.source === 'self-signed')

  /** Why the DoT host name or the DoH address is missing (needed: in a platform's steps). */
  function missing(p: 'dot' | 'doh', needed: boolean): string {
    if (!enc) return t('dns.devices.encryptedUnavailable')
    const s = p === 'dot' ? enc.dot : enc.doh
    if (!s.enabled) {
      if (needed) return t(p === 'dot' ? 'dns.devices.dotNeeded' : 'dns.devices.dohNeeded')
      return t(p === 'dot' ? 'dns.devices.dotOff' : 'dns.devices.dohOff')
    }
    if (!s.serving) return t(p === 'dot' ? 'dns.devices.dotNotServing' : 'dns.devices.dohNotServing', { error: s.error ?? '–' })
    return t(p === 'dot' ? 'dns.devices.dotNoName' : 'dns.devices.dohNoName')
  }

  // In the steps: the first address; unknown values show what to enter instead.
  const v4 = $derived(ipv4[0])
  const v6 = $derived(ula[0])

  const nmIpv4 = $derived(`nmcli connection modify "<connection>" ipv4.dns ${v4 ?? '<PiCache-IPv4>'} ipv4.ignore-auto-dns yes`)
  const nmIpv6 = $derived(v6 ? `nmcli connection modify "<connection>" ipv6.dns ${v6} ipv6.ignore-auto-dns yes` : '')
  const nmUp = 'nmcli connection up "<connection>"'
  const resolved = $derived(dotHost ? `[Resolve]\nDNS=${v4 ?? '<PiCache-IPv4>'}#${dotHost}\nDNSOverTLS=yes` : '')

  const FIREWALL = `${REPO}/blob/main/docs/GUIDES.md#firewall-rules`
  const LETS_ENCRYPT = `${REPO}/blob/main/docs/DEPLOYMENT.md#lets-encrypt`
</script>

{#snippet ipv4Value()}<CopyValue value={v4} missing={t('dns.devices.placeholder.ipv4')} />{/snippet}
{#snippet ulaValue()}<CopyValue value={v6} missing={t('dns.devices.placeholder.ula')} />{/snippet}
{#snippet dotValue()}<CopyValue value={dotHost} missing={t('dns.devices.placeholder.dot')} />{/snippet}
{#snippet dohValue()}<CopyValue value={dohUrl} missing={t('dns.devices.placeholder.doh')} />{/snippet}

<!-- A text whose {ipv4}, {ula}, {dot} and {doh} become the values with copy buttons. -->
{#snippet line(key: MessageKey)}
  <Trans {key}>
    {#snippet ipv4()}{@render ipv4Value()}{/snippet}
    {#snippet ula()}{@render ulaValue()}{/snippet}
    {#snippet dot()}{@render dotValue()}{/snippet}
    {#snippet doh()}{@render dohValue()}{/snippet}
  </Trans>
{/snippet}

{#snippet step(key: MessageKey)}
  <li>{@render line(key)}</li>
{/snippet}

{#snippet ipv6Step(key: MessageKey)}
  {#if v6 || bridge}
    {@render step(key)}
  {:else}
    <li class="muted">{t('dns.devices.noUla')}</li>
  {/if}
{/snippet}

{#snippet command(text: string)}
  <div class="cmd">
    <code class="mono">{text}</code>
    <CopyButton {text} label={t('dns.devices.copyCommand')} />
  </div>
{/snippet}

<!-- The protocol a platform needs does not serve (or is off): why, and where to change it. -->
{#snippet off(p: 'dot' | 'doh')}
  <p class="small muted">
    {missing(p, true)}
    <a href={href('/dns/settings', { section: 'encrypted' })}>{t('dns.devices.encryptedSettings')}</a>
  </p>
{/snippet}

{#snippet browsers()}
  <section class="part">
    <h3>{t('dns.devices.browsers.title')}</h3>
    <p class="small">{t('dns.devices.browsers.text')}</p>
    {#if dohUrl}<p class="small">{@render line('dns.devices.browsers.doh')}</p>{/if}
  </section>
{/snippet}

{#snippet hardcoded()}
  <Notice tone="info" title={t('dns.devices.hardcoded.title')}>
    {t('dns.devices.hardcoded.text')}
    <a href={FIREWALL} target="_blank" rel="noopener noreferrer">
      {t('dns.devices.hardcoded.link')}<Icon name="external" size={14} /><span class="visually-hidden">
        ({t('system.health.docs.newTab')})</span
      >
    </a>
  </Notice>
{/snippet}

{#snippet trust()}
  <p class="small muted">
    {t('dns.devices.trust')}
    <a href={href('/system/https')}>{t('dns.devices.trustLink')}</a>
  </p>
{/snippet}

{#snippet certLink()}
  <a href={LETS_ENCRYPT} target="_blank" rel="noopener noreferrer">
    {t('dns.devices.android.certLink')}<Icon name="external" size={14} /><span class="visually-hidden">
      ({t('system.health.docs.newTab')})</span
    >
  </a>
{/snippet}

<!-- Android's Private DNS uses no CA the user installs: a publicly trusted certificate only. -->
{#snippet publicCert()}
  {#if dotHost && untrustedCert}
    <Notice tone="warn">{t('dns.devices.android.untrusted')} {@render certLink()}</Notice>
  {:else}
    <p class="small muted">{t('dns.devices.android.publicCert')} {@render certLink()}</p>
  {/if}
{/snippet}

<div class="page">
  <div class="top">
    <Button size="sm" variant="ghost" icon="chevron-left" href={href('/dns/network')}>{t('dns.devices.back')}</Button>
  </div>

  <Panel title={t('dns.devices.title')} description={t('dns.devices.intro')}>
    {#if !settled}
      <Skeleton height="96px" />
    {:else}
      <div class="stack">
        {#if check.error && !check.data}
          <Notice tone="warn">{t('dns.devices.unavailable')} {errorText(check.error)}</Notice>
        {/if}
        <dl class="values">
          <dt>{t('dns.devices.ipv4')}</dt>
          <dd>
            {#each ipv4 as a (a)}
              <span class="val"><code class="mono">{a}</code><CopyButton text={a} /></span>
            {:else}
              <span class="muted">{bridge ? t('dns.devices.bridgeAddress') : check.data ? t('dns.devices.noIpv4') : '–'}</span>
            {/each}
          </dd>
          <dt>{t('dns.devices.ula')}</dt>
          <dd>
            {#each ula as a (a)}
              <span class="val"><code class="mono">{a}</code><CopyButton text={a} /></span>
            {:else}
              <span class="muted">{bridge ? t('dns.devices.bridgeAddress') : check.data ? t('dns.devices.noUla') : '–'}</span>
            {/each}
          </dd>
          <dt>{t('dns.devices.dot')}</dt>
          <dd>
            {#if dotHost}
              <span class="val"><code class="mono">{dotHost}</code><CopyButton text={dotHost} /></span>
            {:else}
              <span class="muted">{missing('dot', false)}</span>
            {/if}
          </dd>
          <dt>{t('dns.devices.doh')}</dt>
          <dd>
            {#if dohUrl}
              <span class="val"><code class="mono">{dohUrl}</code><CopyButton text={dohUrl} /></span>
            {:else}
              <span class="muted">{missing('doh', false)}</span>
            {/if}
          </dd>
        </dl>
        <p class="small muted">{t('dns.devices.secondary')}</p>
      </div>
    {/if}
  </Panel>

  <div class="picker">
    <label class="small" for="device-guide-os">{t('dns.devices.platform')}</label>
    <Select id="device-guide-os" value={os} options={tabs.map((x) => ({ value: x.id, label: x.label }))} onchange={(e) => pick(e.currentTarget.value)} />
  </div>

  <div class="guide-tabs">
    <Tabs label={t('dns.devices.platform')} {tabs} active={os} onchange={pick}>
      {#snippet children(active)}
        <div class="guide stack">
          {#if active === 'windows'}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <ol class="steps">
                {@render step('dns.devices.windows.1')}
                {@render step('dns.devices.windows.2')}
                {@render step('dns.devices.windows.3')}
                {@render ipv6Step('dns.devices.windows.4')}
                {@render step('dns.devices.windows.5')}
              </ol>
            </section>
            <section class="part">
              <h3>{t('dns.devices.encrypted')}</h3>
              {#if dohUrl}
                <p class="small">{@render line('dns.devices.windows.doh')}</p>
              {:else}
                {@render off('doh')}
              {/if}
              {@render trust()}
            </section>
            {@render browsers()}
          {:else if active === 'macos' || active === 'ios'}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <ol class="steps">
                {#if active === 'macos'}
                  {@render step('dns.devices.macos.1')}
                  {@render step('dns.devices.macos.2')}
                  {@render ipv6Step('dns.devices.macos.3')}
                  {@render step('dns.devices.macos.4')}
                {:else}
                  {@render step('dns.devices.ios.1')}
                  {@render step('dns.devices.ios.2')}
                  {@render ipv6Step('dns.devices.ios.3')}
                  {@render step('dns.devices.ios.4')}
                {/if}
              </ol>
            </section>
            <section class="part">
              <h3>{t('dns.devices.encrypted')}</h3>
              <p class="small">
                {t('dns.devices.apple.profile')}
                {#if session.canOperate}
                  <a href={href('/dns/settings', { section: 'devices' })}>{t('dns.devices.apple.profileLink')}</a>
                {:else}
                  {t('dns.devices.apple.profileViewer')}
                {/if}
              </p>
              {@render trust()}
            </section>
            {#if active === 'macos'}{@render browsers()}{/if}
          {:else if active === 'android'}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <ol class="steps">
                {@render step('dns.devices.android.1')}
                {@render step('dns.devices.android.2')}
                {@render step('dns.devices.android.3')}
              </ol>
              <p class="small muted">{t('dns.devices.android.private')}</p>
            </section>
            <section class="part">
              <h3>{t('dns.devices.encrypted')}</h3>
              {#if dotHost}
                <p class="small">{@render line('dns.devices.android.dot')}</p>
              {:else}
                {@render off('dot')}
              {/if}
              {@render publicCert()}
            </section>
            {@render browsers()}
          {:else if active === 'linux'}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <p class="small">{t('dns.devices.linux.nm')}</p>
              {@render command(nmIpv4)}
              {#if nmIpv6}{@render command(nmIpv6)}{:else}<p class="small muted">{t('dns.devices.noUla')}</p>{/if}
              {@render command(nmUp)}
              <p class="small muted">{t('dns.devices.linux.check')}</p>
            </section>
            <section class="part">
              <h3>{t('dns.devices.encrypted')}</h3>
              {#if dotHost}
                <p class="small">{t('dns.devices.linux.dot')}</p>
                {@render command(resolved)}
              {:else}
                {@render off('dot')}
              {/if}
              {@render trust()}
            </section>
            {@render browsers()}
          {:else if active === 'chromeos'}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <ol class="steps">
                {@render step('dns.devices.chromeos.1')}
                {@render step('dns.devices.chromeos.2')}
                {@render ipv6Step('dns.devices.chromeos.3')}
              </ol>
            </section>
            <section class="part">
              <h3>{t('dns.devices.encrypted')}</h3>
              <p class="small">{t('dns.devices.chromeos.secure')}</p>
              {#if dohUrl}<p class="small">{@render line('dns.devices.browsers.doh')}</p>{/if}
              {@render trust()}
            </section>
          {:else if active === 'consoles'}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <ul class="steps">
                {@render step('dns.devices.consoles.playstation')}
                {@render step('dns.devices.consoles.xbox')}
                {@render step('dns.devices.consoles.switch')}
              </ul>
              <p class="small muted">{t('dns.devices.consoles.cache')}</p>
            </section>
            {@render hardcoded()}
          {:else}
            <section class="part">
              <h3>{t('dns.devices.manual')}</h3>
              <ul class="steps">
                {@render step('dns.devices.tv.1')}
                {@render step('dns.devices.tv.2')}
                {@render step('dns.devices.tv.3')}
              </ul>
            </section>
            {@render hardcoded()}
          {/if}
          <p class="small muted">
            {t('dns.devices.verify')}
            <a href={href('/dns/queries')}>{t('dns.devices.verifyLink')}</a>
          </p>
        </div>
      {/snippet}
    </Tabs>
  </div>
</div>

<style>
  .top {
    margin-left: calc(-1 * var(--sp-2));
  }
  .values {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--sp-2) var(--sp-4);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    align-self: center;
    color: var(--text-2);
  }
  dd {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
    margin: 0;
    min-width: 0;
  }
  .val {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .picker {
    display: none;
  }
  .guide {
    max-width: 90ch;
  }
  .part {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding-left: var(--sp-5);
    font-size: var(--fs-sm);
  }
  .steps :global(li) {
    overflow-wrap: anywhere;
  }
  .cmd {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .cmd code {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-sm);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  @media (max-width: 640px) {
    .picker {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .picker label {
      font-weight: 600;
    }
    .guide-tabs :global([role='tablist']) {
      display: none;
    }
  }
  @media (max-width: 480px) {
    .values {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    dd {
      margin-bottom: var(--sp-2);
    }
  }
</style>
