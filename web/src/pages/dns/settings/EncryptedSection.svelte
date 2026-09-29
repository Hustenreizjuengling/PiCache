<!--
  @component
  Encrypted DNS for devices: DNS over TLS and DNS over HTTPS with the name
  devices use for them (a one-click suggestion below the local domain),
  their state (./encrypted/EncryptedStatus.svelte), plain DNS on port 53
  (switching it off asks first and warns while PiCache's DHCP server still
  announces it as the DNS server; an error while it stays open because
  nothing encrypted is serving) and notes on certificates and use away
  from home. The switches and the name are saved with the page. Device
  set-up and Apple profiles follow in their own panel. Viewers see the
  state and the addresses; the switches are read-only for them.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ClientGroup, DhcpSettings, DnsSettings, EncryptedDnsStatus, Resource } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, ConfirmDialog, Field, Input, Notice, Panel, Toggle } from '$lib/ui'
  import DeviceSetup from './encrypted/DeviceSetup.svelte'
  import EncryptedStatus from './encrypted/EncryptedStatus.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    /** GET /dns/encrypted (the saved state). */
    status: Resource<EncryptedDnsStatus>
    /** The DHCP settings: does PiCache announce itself as the plain DNS server? */
    dhcp: DhcpSettings | undefined
    /** downloadCache.cacheIpv4 (saved): the address devices use in a container bridge network. */
    cacheIpv4: readonly string[]
    /** For "Add a client with this ClientID". */
    groups: readonly ClientGroup[] | undefined
  }

  let { form, status, dhcp, cacheIpv4, groups }: Props = $props()

  const d = $derived(form.draft as DnsSettings)

  /** picache.<local domain>: the local CA covers it already (offered while the name is empty). */
  const suggestion = $derived.by(() => {
    const domain = d.localDomain.trim().toLowerCase().replace(/\.$/, '')
    return domain ? `picache.${domain}` : ''
  })
  const anyOn = $derived(d.encrypted.dot || d.encrypted.doh)
  /** Plain DNS cannot be switched off while neither DoT nor DoH is on. */
  const plainLocked = $derived(d.plainDns && !anyOn)
  const dhcpAnnounces = $derived(!!dhcp && (dhcp.enabled || dhcp.ipv6.routerAdvertisements || dhcp.ipv6.dhcpv6))
  const protocolError = $derived(form.error('encrypted.dot') ?? form.error('encrypted.doh'))
  const s = $derived(status.data)

  let confirmOpen = $state(false)

  // Switching plain DNS off asks first; switching it on applies at once.
  function setPlain(on: boolean) {
    if (on) d.plainDns = true
    else confirmOpen = true
  }
</script>

<Panel id="dns-set-encrypted" title={t('dns.settings.encrypted.title')} description={t('dns.settings.encrypted.description')}>
  <div class="stack">
    <fieldset class="stack" disabled={!session.isAdmin}>
      <legend class="visually-hidden">{t('dns.settings.encrypted.title')}</legend>
      <div class="stack-sm">
        <Toggle
          bind:checked={d.encrypted.dot}
          id="dns-field-encrypted-dot" label={t('dns.settings.encrypted.dot')}
          description={t('dns.settings.encrypted.dotHelp')}
        />
        <Toggle
          bind:checked={d.encrypted.doh}
          id="dns-field-encrypted-doh" label={t('dns.settings.encrypted.doh')}
          description={t('dns.settings.encrypted.dohHelp')}
        />
        {#if protocolError}<p class="err">{protocolError}</p>{/if}
      </div>
      <div class="name">
        <Field
          id="dns-field-encrypted-serverName" label={t('dns.settings.encrypted.serverName')}
          optional={!anyOn}
          required={anyOn}
          help={t('dns.settings.encrypted.serverNameHelp')}
          error={form.error('encrypted.serverName')}
        >
          <Input
            bind:value={d.encrypted.serverName}
            mono
            placeholder={suggestion || 'dns.example.com'}
            maxlength={253}
            autocomplete="off"
            autocapitalize="off"
            spellcheck={false}
          />
        </Field>
        {#if suggestion && !d.encrypted.serverName.trim()}
          <div>
            <Button size="sm" variant="ghost" onclick={() => (d.encrypted.serverName = suggestion)}>
              {t('dns.settings.encrypted.useSuggestion', { name: suggestion })}
            </Button>
          </div>
        {/if}
      </div>
    </fieldset>

    <EncryptedStatus
      status={s}
      error={status.error}
      loading={status.loading}
      onrefresh={() => status.refresh()}
    />

    <section class="stack-sm sub" aria-labelledby="dns-enc-plain">
      <h3 id="dns-enc-plain">{t('dns.settings.encrypted.plain.title')}</h3>
      <fieldset class="stack-sm" disabled={!session.isAdmin}>
        <legend class="visually-hidden">{t('dns.settings.encrypted.plain.title')}</legend>
        <Toggle
          bind:checked={() => d.plainDns, setPlain}
          disabled={plainLocked}
          id="dns-field-plainDns" label={t('dns.settings.encrypted.plain.label')}
          description={t('dns.settings.encrypted.plain.help')}
        />
      </fieldset>
      {#if plainLocked}<p class="small muted hint">{t('dns.settings.encrypted.plain.enableFirst')}</p>{/if}
      {#if form.error('plainDns')}<p class="err">{form.error('plainDns')}</p>{/if}
      {#if s && !s.plainDns.enabled && s.plainDns.served}
        <Notice tone="fail" title={t('dns.settings.encrypted.plain.stillServedTitle')}>
          {t('dns.settings.encrypted.plain.stillServed')}
        </Notice>
      {/if}
      {#if !d.plainDns && dhcpAnnounces}
        <Notice tone="warn">
          {t('dns.settings.encrypted.plain.dhcpWarning')}
          <a href={href('/dns/dhcp')}>{t('dns.settings.encrypted.plain.openDhcp')}</a>
        </Notice>
      {/if}
    </section>

    <Notice tone="info" icon="lock" title={t('dns.settings.encrypted.notes.title')}>
      <ul class="notes">
        <li>{t('dns.settings.encrypted.notes.publicCert')}</li>
        <li>{t('dns.settings.encrypted.notes.localCa')}</li>
        <li>{t('dns.settings.encrypted.notes.wildcard')}</li>
        <li>{t('dns.settings.encrypted.notes.away')}</li>
      </ul>
    </Notice>
  </div>
</Panel>

<DeviceSetup status={s} {groups} serverNameAddresses={form.saved?.serverNameAddresses.ipv4 ?? []} {cacheIpv4} />

<ConfirmDialog
  bind:open={confirmOpen}
  title={t('dns.settings.encrypted.plain.confirmTitle')}
  message={t('dns.settings.encrypted.plain.confirmText', { name: d.encrypted.serverName.trim() || s?.serverName || '–' })}
  confirmLabel={t('dns.settings.encrypted.plain.confirm')}
  onconfirm={() => {
    if (form.draft) form.draft.plainDns = false
  }}
>
  {#if dhcpAnnounces}
    <Notice tone="warn">{t('dns.settings.encrypted.plain.dhcpWarning')}</Notice>
  {/if}
</ConfirmDialog>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .name {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    max-width: 480px;
  }
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .hint {
    padding-left: calc(36px + var(--sp-3));
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .notes {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding-left: 1.2em;
  }
</style>
