<!--
  @component
  Your own certificate: paste (or pick files with) the certificate chain
  and its unencrypted private key, confirm with your password and PiCache
  serves it at once instead of its local CA. Names and addresses it does
  not cover are listed afterwards (never a reason to refuse it). The key
  is only sent over HTTPS, so on plain HTTP the page links to its HTTPS
  address; with PICACHE_WEB_TLS_CERT set the host's files are used
  instead. A stored upload can be deleted (where the host allows
  destructive actions). The key and the password are dropped after
  sending.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type TlsStatus } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Field, Input, Notice, Panel, Textarea, confirm, toast } from '$lib/ui'
  import { httpsUrl } from '../../auth/https'

  let { status, onchange }: { status: TlsStatus; onchange: (status: TlsStatus) => void } = $props()

  /** Certificate and key together (the server refuses more). */
  const MAX_PEM = 64 * 1024

  let certPem = $state('')
  let keyPem = $state('')
  let password = $state('')
  let submitted = $state(false)
  let busy = $state(false)
  let err = $state.raw<ApiError | null>(null)
  let fileErr = $state('')
  /** After an upload: PiCache's names and addresses the new certificate does not cover. */
  let notCovered = $state.raw<string[]>([])

  /** What is sent: trimmed, with one final newline (the server's limit counts these bytes). */
  const pem = (text: string) => text.trim() + '\n'
  const size = $derived(new TextEncoder().encode(pem(certPem)).length + new TextEncoder().encode(pem(keyPem)).length)
  const errors = $derived({
    cert:
      fieldError(err, 'certPem') ??
      (size > MAX_PEM
        ? t('system.https.upload.tooLarge')
        : submitted && !certPem.includes('-----BEGIN CERTIFICATE-----')
          ? t('system.https.upload.certRequired')
          : undefined),
    key:
      fieldError(err, 'keyPem') ??
      (submitted && !/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(keyPem) ? t('system.https.upload.keyRequired') : undefined),
    password:
      fieldError(err, 'currentPassword') ?? (submitted && !password ? t('system.https.ca.passwordRequired') : undefined),
  })
  const general = $derived(
    err && !['certPem', 'keyPem', 'currentPassword'].includes(err.field ?? '') ? errorText(err) : '',
  )

  // This page on the HTTPS port: the key is only sent over HTTPS.
  const httpsLink = $derived(httpsUrl(location.href, session.status?.httpsPort ?? 0))

  let certInput = $state<HTMLInputElement>()
  let keyInput = $state<HTMLInputElement>()

  /** Reads a picked PEM file into the text area (files larger than the limit are refused). */
  async function pick(e: Event & { currentTarget: HTMLInputElement }, target: 'cert' | 'key') {
    const input = e.currentTarget
    const f = input.files?.[0]
    input.value = '' // picking the same file again fires change again
    fileErr = ''
    if (!f) return
    if (f.size > MAX_PEM) {
      fileErr = t('system.https.upload.fileTooLarge', { name: f.name })
      return
    }
    try {
      const text = await f.text()
      if (target === 'cert') certPem = text
      else keyPem = text
    } catch {
      fileErr = t('system.https.upload.fileUnreadable', { name: f.name })
    }
  }

  async function upload(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = null
    if (errors.cert || errors.key || errors.password) return
    busy = true
    try {
      const st = await api.tls.upload({ certPem: pem(certPem), keyPem: pem(keyPem), currentPassword: password })
      certPem = ''
      keyPem = ''
      password = ''
      submitted = false
      notCovered = st.hostsNotCovered
      onchange(st)
      toast.success(t('system.https.upload.done'))
    } catch (e) {
      err = toApiError(e)
    } finally {
      busy = false
    }
  }

  async function remove() {
    const ok = await confirm({
      title: t('system.https.upload.deleteTitle'),
      message: t('system.https.upload.deleteText'),
      confirmLabel: t('system.https.upload.delete'),
      action: async () => onchange(await api.tls.remove()),
    })
    if (!ok) return
    notCovered = []
    toast.success(t('system.https.upload.deleted'))
  }

  const formId = $props.id()
</script>

{#snippet submitFooter()}
  <Button type="submit" form="tls-{formId}" variant="primary" icon="upload" loading={busy}>
    {t('system.https.upload.submit')}
  </Button>
{/snippet}

<Panel
  title={t('system.https.upload.title')}
  description={t('system.https.upload.description')}
  footer={status.upload.allowed && session.isAdmin ? submitFooter : undefined}
>
  <div class="stack">
    {#if notCovered.length > 0}
      <Notice tone="warn" title={t('system.https.upload.notCoveredTitle')} ondismiss={() => (notCovered = [])}>
        <p>{t('system.https.upload.notCoveredText')}</p>
        <p class="mono">{notCovered.join(', ')}</p>
      </Notice>
    {/if}

    {#if status.uploadStored}
      <div class="stored">
        <p class="small">
          {status.envOverride
            ? t('system.https.upload.storedUnused')
            : status.source === 'uploaded'
              ? t('system.https.upload.stored')
              : t('system.https.upload.storedBroken')}
        </p>
        {#if session.canDestroy}
          <Button size="sm" variant="danger" icon="trash" onclick={remove}>{t('system.https.upload.delete')}</Button>
        {/if}
      </div>
    {/if}

    {#if !status.upload.allowed && status.upload.reason === 'env-override'}
      <Notice tone="info">{t('system.https.upload.envOverride')}</Notice>
    {:else if !status.upload.allowed && status.upload.reason === 'plain-http'}
      <Notice tone="info" title={t('system.https.upload.plainTitle')}>
        <p>{t('system.https.upload.plainText')}</p>
        {#if httpsLink}<p><a class="mono link" href={httpsLink}>{httpsLink}</a></p>{/if}
      </Notice>
    {:else if status.upload.allowed && session.isAdmin}
      <form id="tls-{formId}" class="stack" onsubmit={upload} novalidate>
        {#if general}<Notice tone="fail">{general}</Notice>{/if}
        {#if fileErr}<Notice tone="fail">{fileErr}</Notice>{/if}
        <Field label={t('system.https.upload.cert')} error={errors.cert} help={t('system.https.upload.certHelp')}>
          <Textarea bind:value={certPem} rows={6} mono spellcheck={false} placeholder={'-----BEGIN CERTIFICATE-----\n…'} />
        </Field>
        <div class="file">
          <input bind:this={certInput} class="visually-hidden" type="file" accept=".pem,.crt,.cer" tabindex="-1" aria-hidden="true" onchange={(e) => pick(e, 'cert')} />
          <Button size="sm" icon="upload" onclick={() => certInput?.click()}>{t('system.https.upload.pickCert')}</Button>
        </div>
        <Field label={t('system.https.upload.key')} error={errors.key} help={t('system.https.upload.keyHelp')}>
          <Textarea bind:value={keyPem} rows={5} mono spellcheck={false} autocomplete="off" placeholder={'-----BEGIN PRIVATE KEY-----\n…'} />
        </Field>
        <div class="file">
          <input bind:this={keyInput} class="visually-hidden" type="file" accept=".pem,.key" tabindex="-1" aria-hidden="true" onchange={(e) => pick(e, 'key')} />
          <Button size="sm" icon="upload" onclick={() => keyInput?.click()}>{t('system.https.upload.pickKey')}</Button>
        </div>
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
        <Field label={t('system.users.current')} error={errors.password}>
          <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
        </Field>
      </form>
    {/if}

    <p class="small muted">{t('system.https.upload.letsEncrypt')}</p>
  </div>
</Panel>

<style>
  .stored {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .link {
    overflow-wrap: anywhere;
  }
  .file {
    margin-top: calc(-1 * var(--sp-2));
  }
</style>
