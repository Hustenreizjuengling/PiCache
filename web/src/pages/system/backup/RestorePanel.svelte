<!--
  @component
  Restore a backup: pick a file (checked in the browser for size and the
  SQLite header), confirm with the current password, upload. The server
  verifies the password and the file and stages it for the next start;
  "Restart now" applies it. The password is dropped when the dialog closes.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type RestoreResult } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Dialog, Field, Input, Notice, Panel, toast } from '$lib/ui'
  import RestartButton from '../RestartButton.svelte'
  import { checkBackupFile } from './file'

  let input = $state<HTMLInputElement>()
  let file = $state.raw<File | undefined>(undefined)
  let fileError = $state('')
  let uploading = $state(false)
  let result = $state.raw<RestoreResult | undefined>(undefined)
  let uploadErr = $state.raw<ApiError | undefined>(undefined)

  async function picked() {
    const f = input?.files?.[0]
    if (input) input.value = '' // choosing the same file again fires change again
    if (!f) return
    result = undefined
    uploadErr = undefined
    const problem = await checkBackupFile(f)
    fileError = problem ? t(`system.backup.restore.check.${problem}`) : ''
    file = f
  }

  // ---- confirmation with the current password
  let confirmOpen = $state(false)
  let password = $state('')
  let passwordSubmitted = $state(false)
  let passwordErr = $state.raw<ApiError | null>(null)
  const passwordError = $derived(
    passwordErr?.field === 'password'
      ? t('system.backup.restore.passwordWrong')
      : passwordSubmitted && !password
        ? t('system.backup.restore.passwordRequired')
        : undefined,
  )
  const passwordGeneral = $derived(passwordErr && !passwordErr.field ? errorText(passwordErr) : '')

  function restore() {
    if (!file || fileError) return
    password = ''
    passwordSubmitted = false
    passwordErr = null
    confirmOpen = true
  }

  async function upload(e: SubmitEvent) {
    e.preventDefault()
    const f = file
    passwordSubmitted = true
    passwordErr = null
    if (!f || !password) return
    uploading = true
    uploadErr = undefined
    try {
      result = await api.system.restore(f, password)
      confirmOpen = false
      file = undefined
      toast.success(t('system.backup.restore.staged'))
    } catch (err) {
      const ae = toApiError(err)
      if (ae.field === 'password' || ae.code === 'too_many_requests') {
        passwordErr = ae // stays in the dialog: try again
      } else {
        confirmOpen = false
        uploadErr = ae
      }
    } finally {
      uploading = false
    }
  }

  function confirmClosed() {
    password = ''
    passwordSubmitted = false
    passwordErr = null
  }

  const formId = $props.id()

</script>

<Panel title={t('system.backup.restore.title')} description={t('system.backup.restore.description')}>
  <div class="stack">
    {#if result}
      <Notice tone="ok" title={t('system.backup.restore.stagedTitle')}>
        <p>{t('system.backup.restore.stagedText')}</p>
        {#snippet actions()}
          <RestartButton
            variant="primary"
            label={t('system.backup.restore.restartNow')}
            message={t('system.backup.restore.restartText')}
          />
        {/snippet}
      </Notice>
    {/if}

    <div class="pick">
      <input
        bind:this={input}
        class="visually-hidden"
        type="file"
        accept=".db,.sqlite,.sqlite3,application/octet-stream,application/vnd.sqlite3,application/x-sqlite3"
        tabindex="-1"
        aria-hidden="true"
        onchange={picked}
      />
      <Button icon="upload" disabled={!session.isAdmin || uploading} onclick={() => input?.click()}>
        {file ? t('system.backup.restore.chooseOther') : t('system.backup.restore.choose')}
      </Button>
      {#if file}
        <span class="file">
          <span class="mono truncate" title={file.name}>{file.name}</span>
          <span class="small muted nowrap">{formatBytes(file.size)}</span>
        </span>
      {/if}
    </div>

    {#if fileError}
      <Notice tone="fail" title={t('system.backup.restore.checkFailed')}>{fileError}</Notice>
    {/if}
    {#if uploadErr}
      <Notice tone="fail" title={t('system.backup.restore.rejected')}>{errorText(uploadErr)}</Notice>
    {/if}

    <ul class="facts small muted">
      <li>{t('system.backup.restore.factReplace')}</li>
      <li>{t('system.backup.restore.factSessions')}</li>
      <li>{t('system.backup.restore.factKeep')}</li>
    </ul>

    <div class="row">
      <Button
        variant={result ? 'secondary' : 'primary'}
        icon="archive"
        loading={uploading}
        disabled={!file || !!fileError || !session.isAdmin}
        onclick={restore}
      >
        {uploading ? t('system.backup.restore.uploading') : t('system.backup.restore.button')}
      </Button>
    </div>
  </div>
  {#snippet footer()}
    <p class="small muted grow">{t('system.backup.restore.restartHint')}</p>
    {#if !result}<RestartButton size="sm" />{/if}
  {/snippet}
</Panel>

<Dialog
  bind:open={confirmOpen}
  title={t('system.backup.restore.confirmTitle', { name: file?.name ?? '' })}
  size="sm"
  dismissible={!uploading}
  onclose={confirmClosed}
>
  <form id="restore-{formId}" class="stack" onsubmit={upload} novalidate>
    <p class="small muted">{t('system.backup.restore.confirmText')}</p>
    {#if passwordGeneral}<Notice tone="fail">{passwordGeneral}</Notice>{/if}
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
    <Field
      label={t('system.backup.restore.password')}
      error={passwordError}
      help={t('system.backup.restore.passwordHelp')}
    >
      <Input type="password" bind:value={password} autocomplete="current-password" maxlength={1024} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button disabled={uploading} onclick={() => (confirmOpen = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="restore-{formId}" variant="primary" icon="archive" loading={uploading}>
      {uploading ? t('system.backup.restore.uploading') : t('system.backup.restore.confirm')}
    </Button>
  {/snippet}
</Dialog>

<style>
  .pick {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    min-width: 0;
  }
  .file {
    display: inline-flex;
    align-items: baseline;
    gap: var(--sp-2);
    min-width: 0;
    max-width: 100%;
  }
  .facts {
    margin: 0;
    padding-left: var(--sp-5);
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
  }
  .grow {
    flex: 1 1 240px;
  }
</style>
