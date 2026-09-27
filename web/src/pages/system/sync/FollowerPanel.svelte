<!--
  @component
  Sets this PiCache up as a follower (settings.sync): off or follower, the
  primary's address (https), a sync token of the primary (write-only: kept
  until the address changes), the primary's CA certificate as the trust
  anchor (pasted or loaded from a file; empty = the system roots), the
  interval and the sections it copies (with the dependency rule). The
  chosen sections become read-only here and are replaced at every sync.
  Read-only for admins while the host locks the configuration.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { SYNC_SECTIONS, type SyncSettings } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Field, Input, Notice, Panel, Segmented, Skeleton, Textarea, toast } from '$lib/ui'
  import NumberInput from '../../dns/shared/NumberInput.svelte'
  import { rangeError, RANGES } from '../forms'
  import SectionPicker from '../SectionPicker.svelte'

  let { onsaved }: { onsaved?: () => void } = $props()

  const form = settingsForm('sync')

  /** A new token (write-only: the stored one is never shown). */
  let token = $state('')
  let fileInput = $state<HTMLInputElement>()
  let fileError = $state('')

  const d = $derived(form.draft as SyncSettings | undefined)
  const s = $derived(form.saved)
  const follower = $derived(d?.mode === 'follower')
  /** The stored token belongs to the saved address: a new address needs it again. */
  const sourceChanged = $derived(!!d && !!s && s.tokenSet && d.source.trim() !== s.source)
  const dirty = $derived(form.dirty || !!token)
  const invalid = $derived(!!d && follower && !!rangeError(d.intervalMinutes, RANGES.syncIntervalMinutes))
  const general = $derived(form.saveError && !form.saveError.field ? form.errorMessage : undefined)

  const MAX_PEM = 16 * 1024

  async function loadFile() {
    const f = fileInput?.files?.[0]
    if (fileInput) fileInput.value = ''
    if (!f || !form.draft) return
    fileError = ''
    if (f.size > MAX_PEM) {
      fileError = t('system.sync.form.caTooLarge')
      return
    }
    const text = await f.text()
    if (!text.includes('-----BEGIN CERTIFICATE-----')) {
      fileError = t('system.sync.form.caNotPem')
      return
    }
    form.draft.caPem = text.trim()
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    const draft = form.draft
    if (!draft || invalid) return
    draft.source = draft.source.trim()
    draft.caPem = draft.caPem.trim()
    // Write-only: absent keeps the stored token.
    if (token) draft.token = token
    const ok = await form.save()
    if (ok) {
      token = ''
      toast.success(follower ? t('system.sync.form.savedFollower') : t('system.sync.form.savedOff'))
      onsaved?.()
    } else if (form.draft) {
      delete form.draft.token
    }
  }

  function discard() {
    token = ''
    fileError = ''
    form.revert()
  }

  const formId = $props.id()
</script>

{#snippet footer()}
  <div class="row">
    <Button type="submit" form="sync-{formId}" variant="primary" loading={form.saving} disabled={!dirty || invalid}>{t('common.action.save')}</Button>
    <Button variant="ghost" disabled={!dirty || form.saving} onclick={discard}>{t('system.form.discard')}</Button>
  </div>
{/snippet}

<Panel title={t('system.sync.form.title')} description={t('system.sync.form.description')} footer={d && session.isAdmin ? footer : undefined}>
  {#if form.loadError && !d}
    <Notice tone="fail" title={t('system.sync.form.loadError')}>{errorText(form.loadError)}</Notice>
  {:else if !d}
    <Skeleton height="360px" />
  {:else}
    <form id="sync-{formId}" onsubmit={save} novalidate>
      <fieldset class="stack" disabled={!session.isAdmin}>
        {#if general}<Notice tone="fail">{general}</Notice>{/if}
        <div class="stack-sm">
          <Segmented
            label={t('system.sync.form.mode')}
            value={d.mode}
            options={[
              { value: 'off', label: t('system.sync.form.modeOff') },
              { value: 'follower', label: t('system.sync.form.modeFollower') },
            ]}
            onchange={(v) => form.draft && (form.draft.mode = v === 'follower' ? 'follower' : 'off')}
          />
          {#if form.error('mode')}<p class="err">{form.error('mode')}</p>{/if}
          {#if !follower}<p class="small muted">{t('system.sync.form.offText')}</p>{/if}
        </div>

        {#if follower}
          <Field label={t('system.sync.form.source')} required help={t('system.sync.form.sourceHelp')} error={form.error('source')}>
            <Input bind:value={d.source} mono maxlength={2048} autocomplete="off" inputmode="url" placeholder="https://picache.fritz.box:8443" />
          </Field>
          <Field
            label={t('system.sync.form.token')}
            required={!s?.tokenSet}
            help={sourceChanged && !token ? t('system.sync.form.tokenAgain') : t('system.sync.form.tokenHelp')}
            error={form.error('token')}
          >
            <Input
              type="password"
              bind:value={token}
              maxlength={256}
              autocomplete="off"
              placeholder={s?.tokenSet ? t('system.notifications.form.secretKeep') : undefined}
            />
          </Field>
          <div class="stack-sm">
            <Field label={t('system.sync.form.ca')} optional help={t('system.sync.form.caHelp')} error={form.error('caPem') ?? (fileError || undefined)}>
              <Textarea bind:value={d.caPem} rows={5} mono spellcheck="false" placeholder={'-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----'} />
            </Field>
            <div class="row">
              <input
                bind:this={fileInput}
                class="visually-hidden"
                type="file"
                accept=".crt,.pem,.cer,application/x-x509-ca-cert,application/x-pem-file"
                tabindex="-1"
                aria-hidden="true"
                onchange={loadFile}
              />
              <Button size="sm" icon="upload" onclick={() => fileInput?.click()}>{t('system.sync.form.caLoad')}</Button>
              {#if d.caPem}
                <Button size="sm" variant="ghost" onclick={() => form.draft && (form.draft.caPem = '')}>{t('system.sync.form.caClear')}</Button>
              {/if}
            </div>
          </div>
          <Field label={t('system.sync.form.interval')} help={t('system.sync.form.intervalHelp')} error={form.error('intervalMinutes') ?? (invalid ? rangeError(d.intervalMinutes, RANGES.syncIntervalMinutes) : undefined)}>
            <NumberInput bind:value={d.intervalMinutes} min={RANGES.syncIntervalMinutes.min} max={RANGES.syncIntervalMinutes.max} unit={t('system.sync.form.minutes')} />
          </Field>
          <SectionPicker all={SYNC_SECTIONS} bind:value={d.sections} legend={t('system.sync.form.sections')} error={form.error('sections')} />
          <Notice tone="warn" title={t('system.sync.form.replaceTitle')}>{t('system.sync.form.replaceText')}</Notice>
        {/if}
      </fieldset>
    </form>
  {/if}
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
