<!--
  @component
  Apple configuration profiles (iPhone, iPad, Mac) for DoH or DoT (only
  protocols that are serving): an optional ClientID, the home Wi-Fi names
  the profile applies on (at least one, unless "Use everywhere", which only
  suits devices with a VPN) and PiCache's addresses. "Create link" gives a
  15-minute link on the server name with a QR code (links up to 213
  bytes) to open on the device; "Download profile" saves the file (only
  when this page came over HTTPS). Works while the host locks the
  configuration: it changes nothing. Checks as typed; the server's field
  errors appear at the inputs.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    toApiError,
    type ApiError,
    type EncryptedDnsStatus,
    type ProfileLink,
    type ProfileOptions,
    type ProfileProtocol,
  } from '$lib/api'
  import { saveBlob } from '$lib/download'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime, formatTime } from '$lib/format'
  import { Button, Checkbox, CopyButton, Field, Input, Notice, Segmented, toast } from '$lib/ui'
  import QrCode from '$lib/ui/QrCode.svelte'
  import LinesInput from '../../shared/LinesInput.svelte'
  import { isClientId, normalizeClientId } from '../../shared/clientid'
  import { MAX_SSIDS, QR_MAX_BYTES, utf8Length, validSsids } from './endpoints'

  let { status }: { status: EncryptedDnsStatus } = $props()

  const FIELDS = ['protocol', 'dnsClientId', 'ssids']

  let protocol = $state<ProfileProtocol>(untrack(() => (status.doh.serving || !status.dot.serving ? 'doh' : 'dot')))
  let clientIdText = $state('')
  let ssids = $state<string[]>([])
  let everywhere = $state(false)
  let addresses = $state(false)
  let submitted = $state(false)
  let busy = $state<'' | 'link' | 'download'>('')
  let err = $state.raw<ApiError | undefined>(undefined)
  let link = $state.raw<ProfileLink | undefined>(undefined)

  const serving = $derived({ doh: status.doh.serving, dot: status.dot.serving })
  const anyServing = $derived(serving.doh || serving.dot)

  // The chosen protocol stopped serving: take the other one.
  $effect(() => {
    const s = serving
    untrack(() => {
      if (!s[protocol] && s[protocol === 'doh' ? 'dot' : 'doh']) protocol = protocol === 'doh' ? 'dot' : 'doh'
    })
  })

  const clientId = $derived(normalizeClientId(clientIdText))
  const options = $derived<ProfileOptions>({
    protocol,
    dnsClientId: clientId || undefined,
    ssids: everywhere ? undefined : ssids,
    addresses: addresses || undefined,
  })

  // A link belongs to the options it was made with.
  $effect(() => {
    void JSON.stringify(options)
    untrack(() => {
      link = undefined
      err = undefined
    })
  })

  const errors = $derived({
    protocol: fieldError(err, 'protocol'),
    clientId: clientId && !isClientId(clientId) ? t('dns.settings.encrypted.clientIdInvalid') : fieldError(err, 'dnsClientId'),
    ssids: everywhere
      ? undefined
      : !validSsids(ssids)
        ? t('dns.settings.encrypted.profile.ssidsInvalid', { max: MAX_SSIDS })
        : submitted && ssids.length === 0
          ? t('dns.settings.encrypted.profile.ssidsRequired')
          : fieldError(err, 'ssids'),
  })
  const general = $derived(err && !FIELDS.includes(err.field ?? '') ? errorText(err) : '')
  /** The profile file is only offered to a page that came over HTTPS (the server refuses plain HTTP). */
  const canDownload = location.protocol === 'https:'
  const qrFits = $derived(!!link && utf8Length(link.url) <= QR_MAX_BYTES)

  const protocolOptions = $derived([
    { value: 'doh', label: t('dns.settings.encrypted.dohShort'), disabled: !serving.doh },
    { value: 'dot', label: t('dns.settings.encrypted.dotShort'), disabled: !serving.dot },
  ])

  function valid(): boolean {
    submitted = true
    err = undefined
    return !errors.clientId && !errors.ssids
  }

  async function createLink() {
    if (!valid()) return
    busy = 'link'
    try {
      link = await api.dns.createProfileLink(options)
    } catch (e) {
      err = toApiError(e)
    } finally {
      busy = ''
    }
  }

  async function download() {
    if (!valid()) return
    busy = 'download'
    try {
      const f = await api.dns.profile(options)
      saveBlob(f.blob, f.filename ?? `picache-${protocol}${clientId ? `-${clientId}` : ''}.mobileconfig`)
      toast.success(t('dns.settings.encrypted.profile.downloaded'))
    } catch (e) {
      err = toApiError(e)
    } finally {
      busy = ''
    }
  }
</script>

<section class="stack-sm sub" aria-labelledby="dns-enc-profile">
  <h3 id="dns-enc-profile">{t('dns.settings.encrypted.profile.title')}</h3>
  <p class="small muted">{t('dns.settings.encrypted.profile.description')}</p>

  {#if !anyServing}
    <p class="small muted">{t('dns.settings.encrypted.profile.notServing')}</p>
  {:else}
    <div class="form stack">
      <Field label={t('dns.settings.encrypted.profile.protocol')} error={errors.protocol}>
        <Segmented
          label={t('dns.settings.encrypted.profile.protocol')}
          value={protocol}
          options={protocolOptions}
          onchange={(v) => (protocol = v === 'dot' ? 'dot' : 'doh')}
        />
      </Field>
      <Field label={t('dns.settings.encrypted.clientId')} optional help={t('dns.settings.encrypted.profile.clientIdHelp')} error={errors.clientId}>
        <Input bind:value={clientIdText} mono maxlength={63} placeholder="mias-iphone" autocomplete="off" autocapitalize="off" spellcheck={false} />
      </Field>
      <div class="stack-sm">
        <Field
          label={t('dns.settings.encrypted.profile.ssids')}
          required={!everywhere}
          help={t('dns.settings.encrypted.profile.ssidsHelp', { max: MAX_SSIDS })}
          error={errors.ssids}
        >
          <LinesInput bind:value={ssids} rows={2} mono={false} disabled={everywhere} placeholder={t('dns.settings.encrypted.profile.ssidsPlaceholder')} />
        </Field>
        <Checkbox bind:checked={everywhere} label={t('dns.settings.encrypted.profile.everywhere')} />
        {#if everywhere}
          <Notice tone="warn">{t('dns.settings.encrypted.profile.everywhereWarning')}</Notice>
        {/if}
      </div>
      <Checkbox
        bind:checked={addresses}
        label={t('dns.settings.encrypted.profile.addresses')}
        description={t('dns.settings.encrypted.profile.addressesHelp')}
      />
      {#if general}<Notice tone="fail">{general}</Notice>{/if}
      <div class="row">
        <Button variant="primary" icon="link" loading={busy === 'link'} disabled={!!busy} onclick={createLink}>
          {t('dns.settings.encrypted.profile.createLink')}
        </Button>
        {#if canDownload}
          <Button icon="download" loading={busy === 'download'} disabled={!!busy} onclick={download}>
            {t('dns.settings.encrypted.profile.download')}
          </Button>
        {/if}
      </div>
    </div>

    {#if link}
      <div class="link" aria-live="polite">
        {#if qrFits}
          <QrCode text={link.url} label={t('dns.settings.encrypted.profile.qrLabel')} fallback="" size={176} />
        {/if}
        <div class="stack-sm linktext">
          <p class="small strong">{t('dns.settings.encrypted.profile.linkReady')}</p>
          <span class="url">
            <code class="mono">{link.url}</code>
            <CopyButton text={link.url} />
          </span>
          <p class="small muted" title={formatDateTime(link.expiresAt)}>
            {t('dns.settings.encrypted.profile.validUntil', { time: formatTime(link.expiresAt) })}
          </p>
          <p class="small">{t('dns.settings.encrypted.profile.instructions')}</p>
        </div>
      </div>
    {/if}
  {/if}
</section>

<style>
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .form {
    max-width: 480px;
  }
  .link {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-4);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .linktext {
    flex: 1 1 260px;
    min-width: 0;
    max-width: 70ch;
  }
  .strong {
    font-weight: 600;
  }
  .url {
    display: flex;
    align-items: center;
    gap: 2px;
    min-width: 0;
  }
  .url code {
    padding: 1px 6px;
    border: 1px solid var(--line);
    border-radius: 4px;
    background: var(--surface);
    overflow-wrap: anywhere;
  }
</style>
