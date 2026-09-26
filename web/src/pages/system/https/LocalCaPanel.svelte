<!--
  @component
  PiCache's local certificate authority: download its certificate (trusted
  once per device, it makes browsers accept PiCache's own certificate), its
  subject, expiry and the names and addresses it may issue certificates
  for. When PiCache has names or addresses outside those, or still serves
  the self-signed certificate of an older version, it offers to create a
  (new) local CA; a new CA must be trusted on every device again. Creating
  one asks for the admin's password.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type TlsStatus } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDate } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, CopyButton, Dialog, Field, Input, KeyValue, Notice, Panel, toast } from '$lib/ui'
  import { daysBefore, expiryTone, SWITCH_DAYS } from './cert'

  let { status, onchange }: { status: TlsStatus; onchange: (status: TlsStatus) => void } = $props()

  const ca = $derived(status.localCa)
  const cert = $derived(status.certificate)
  /**
   * The self-signed certificate of an older version is still served: it is
   * replaced on this date. Not for the temporary certificate PiCache serves
   * while it cannot store one (the status then carries an error).
   */
  const switchDate = $derived(
    status.source === 'self-signed' && cert && !status.error ? daysBefore(cert.notAfter, SWITCH_DAYS) : undefined,
  )
  /** Certificate files or an upload are in use: a local CA is prepared for later. */
  const otherSource = $derived(status.source === 'files' || status.source === 'uploaded')
  /** The CA serves and misses some of PiCache's names or addresses ("Not covered" above lists them). */
  const renewNow = $derived(!!ca?.renewalNeeded && status.source === 'local-ca')
  /** A prepared CA (another source serves) misses some of them: the list above is about that source. */
  const renewLater = $derived(!!ca?.renewalNeeded && status.source !== 'local-ca')

  // ---- create (or replace) the local CA
  let open = $state(false)
  let password = $state('')
  let submitted = $state(false)
  let busy = $state(false)
  let err = $state.raw<ApiError | null>(null)
  const passwordError = $derived(
    fieldError(err, 'currentPassword') ?? (submitted && !password ? t('system.https.ca.passwordRequired') : undefined),
  )
  const general = $derived(err && err.field !== 'currentPassword' ? errorText(err) : '')

  function ask() {
    password = ''
    submitted = false
    err = null
    open = true
  }

  async function create(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = null
    if (!password) return
    busy = true
    try {
      const replaced = !!ca
      const st = await api.tls.createLocalCa(password)
      password = ''
      open = false
      onchange(st)
      toast.success(t(replaced ? 'system.https.ca.replaced' : 'system.https.ca.created'))
    } catch (e) {
      err = toApiError(e)
    } finally {
      busy = false
    }
  }

  const formId = $props.id()
</script>

<Panel title={t('system.https.ca.title')} description={t('system.https.ca.description')}>
  {#snippet actions()}
    {#if status.caAvailable}
      <Button variant="primary" icon="download" href={api.tls.caUrl()} download="picache-ca.crt">
        {t('system.https.ca.download')}
      </Button>
    {/if}
  {/snippet}

  <div class="stack">
    {#if renewNow}
      <Notice tone="warn" title={t('system.https.ca.renewTitle')}>
        <p>{t('system.https.ca.renewText')}</p>
        {#snippet actions()}
          {#if session.isAdmin}
            <Button size="sm" icon="refresh" onclick={ask}>{t('system.https.ca.createNew')}</Button>
          {/if}
        {/snippet}
      </Notice>
    {/if}

    {#if switchDate}
      <Notice tone="info" title={t('system.https.ca.legacyTitle')}>
        <p>{t('system.https.ca.legacyText', { date: formatDate(switchDate) })}</p>
        {#snippet actions()}
          {#if session.isAdmin}
            <Button size="sm" icon="shield-check" onclick={ask}>{t('system.https.ca.createNow')}</Button>
          {/if}
        {/snippet}
      </Notice>
    {/if}

    {#if ca}
      <div class="head">
        <Badge tone={expiryTone(ca.notAfter, 90)}>{t('system.https.ca.validUntil', { date: formatDate(ca.notAfter) })}</Badge>
        {#if status.source === 'local-ca'}<Badge tone="info">{t('system.https.ca.inUse')}</Badge>{/if}
      </div>
      <KeyValue items={[{ label: t('system.https.cert.subject'), value: ca.subject, mono: true }]}>
        <dt>{t('system.https.ca.names')}</dt>
        <dd>
          {#if ca.permittedNames.length > 0}
            <ul class="list">
              {#each ca.permittedNames as n (n)}<li class="mono">{n}</li>{/each}
            </ul>
          {:else}
            <span class="subtle">–</span>
          {/if}
        </dd>
        <dt>{t('system.https.ca.addresses')}</dt>
        <dd>
          {#if ca.permittedAddresses.length > 0}
            <ul class="list">
              {#each ca.permittedAddresses as a (a)}<li class="mono">{a}</li>{/each}
            </ul>
          {:else}
            <span class="subtle">–</span>
          {/if}
        </dd>
        <dt>{t('system.https.cert.fingerprint')}</dt>
        <dd class="fp">
          <code class="mono">{ca.fingerprintSha256}</code>
          <CopyButton text={ca.fingerprintSha256} label={t('system.https.cert.copyFingerprint')} />
        </dd>
      </KeyValue>
      <p class="small muted">{t('system.https.ca.constraints')}</p>
      {#if otherSource}<p class="small muted">{t('system.https.ca.prepared')}</p>{/if}
      {#if renewLater}<p class="small">{t('system.https.ca.renewPrepared')}</p>{/if}
      {#if session.isAdmin && !renewNow}
        <div class="row">
          <Button icon="refresh" onclick={ask}>{t('system.https.ca.createNew')}</Button>
        </div>
      {/if}
    {:else if !switchDate}
      <p class="small muted">{otherSource ? t('system.https.ca.noneOther') : t('system.https.ca.none')}</p>
      {#if session.isAdmin}
        <div class="row">
          <Button icon="shield-check" onclick={ask}>{t('system.https.ca.create')}</Button>
        </div>
      {/if}
    {/if}
  </div>
</Panel>

<Dialog
  bind:open
  title={ca ? t('system.https.ca.createNewTitle') : t('system.https.ca.createTitle')}
  size="sm"
  dismissible={!busy}
  onclose={() => (password = '')}
>
  <form id="ca-{formId}" class="stack" onsubmit={create} novalidate>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}
    {#if ca}
      <Notice tone="warn">{t('system.https.ca.replaceWarn')}</Notice>
    {:else}
      <p class="small muted">{t('system.https.ca.createText')}</p>
    {/if}
    <!-- Lets password managers offer the right account's password. -->
    <input
      class="visually-hidden"
      type="text"
      name="username"
      autocomplete="username"
      value={session.user?.username ?? ''}
      readonly
      tabindex="-1"
      aria-hidden="true"
    />
    <Field label={t('system.users.current')} error={passwordError}>
      <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={busy} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="ca-{formId}" variant={ca ? 'danger' : 'primary'} icon="shield-check" loading={busy}>
      {ca ? t('system.https.ca.createNew') : t('system.https.ca.create')}
    </Button>
  {/snippet}
</Dialog>

<style>
  .head {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
  .list {
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
</style>
