<!--
  @component
  Support bundle (admins): a zip of redacted diagnostics for a bug report
  (version, settings, health, listeners, network check, DHCP status,
  databases, host resources, the application log). The dialog asks for the
  password (never put in the URL) and whether names and private addresses
  may stay; the file is fetched and saved from memory.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError } from '$lib/api'
  import { fileStamp, saveBlob } from '$lib/download'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, Checkbox, Dialog, Field, Input, Notice, Panel, toast } from '$lib/ui'

  let open = $state(false)
  let password = $state('')
  let includeNames = $state(false)
  let busy = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)

  const formId = $props.id()
  const pwError = $derived(fieldError(err, 'currentPassword'))
  const general = $derived(err && !pwError ? errorText(err) : undefined)

  function start() {
    password = ''
    includeNames = false
    err = undefined
    open = true
  }

  async function create(e: SubmitEvent) {
    e.preventDefault()
    if (busy) return
    if (!password) {
      err = undefined
      document.getElementById(`bundle-pw-${formId}`)?.focus()
      return
    }
    busy = true
    err = undefined
    try {
      const f = await api.system.supportBundle({ currentPassword: password, includeClientNames: includeNames })
      saveBlob(f.blob, f.filename ?? `picache-support-${fileStamp()}.zip`)
      password = ''
      open = false
      toast.success(t('system.health.bundle.done'))
    } catch (x) {
      err = toApiError(x)
    } finally {
      busy = false
    }
  }
</script>

<Panel id="support" title={t('system.health.bundle.title')} description={t('system.health.bundle.description')}>
  <div class="row">
    <p class="small muted grow">{t('system.health.bundle.never')}</p>
    <Button icon="archive" onclick={start}>{t('system.health.bundle.button')}</Button>
  </div>
</Panel>

<Dialog bind:open title={t('system.health.bundle.dialogTitle')} dismissible={!busy}>
  <form id="bundle-{formId}" class="stack" onsubmit={create} novalidate>
    <p>{t('system.health.bundle.intro')}</p>
    <Notice tone="warn">{t('system.health.bundle.review')}</Notice>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}
    <Checkbox bind:checked={includeNames} label={t('system.health.bundle.names')} description={t('system.health.bundle.namesHelp')} disabled={busy} />
    <Field id="bundle-pw-{formId}" label={t('system.health.bundle.password')} required error={pwError}>
      <Input type="password" bind:value={password} autocomplete="current-password" disabled={busy} />
    </Field>
  </form>
  {#snippet actions()}
    <Button variant="ghost" disabled={busy} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button variant="primary" type="submit" form="bundle-{formId}" icon="download" loading={busy} disabled={!password}>
      {t('system.health.bundle.submit')}
    </Button>
  {/snippet}
</Dialog>

<style>
  .grow {
    flex: 1 1 280px;
  }
</style>
