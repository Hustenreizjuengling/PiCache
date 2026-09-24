<!--
  @component
  Web interface settings (settings.web): session lifetime, extra host names,
  the HTTPS redirect and the default language. The metrics switch lives on
  the API tokens page. Saving only sends the changed members.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type WebSettings } from '$lib/api'
  import { formatDuration } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Checkbox, Field, Input, Notice, Panel, Select, Skeleton, Textarea, toast } from '$lib/ui'
  import { MAX_ALLOWED_HOSTS, parseHostList, rangeError, RANGES } from '../forms'

  const form = settingsForm('web')
  const info = resource((signal) => api.system.info({ signal }))

  // The host list is edited as text; the draft gets the parsed entries.
  let hostsText = $state('')
  $effect(() => {
    const saved = form.saved
    if (saved) hostsText = saved.allowedHosts.join('\n')
  })

  function setHosts(v: string) {
    hostsText = v
    if (form.draft) form.draft.allowedHosts = parseHostList(v)
    // An error about a host names an index of the list as it was saved.
    if (form.saveError?.field?.startsWith('web.allowedHosts')) form.saveError = undefined
  }

  function revert() {
    form.revert()
    hostsText = form.saved?.allowedHosts.join('\n') ?? ''
  }

  // HTTPS: the redirect needs a working HTTPS listener, or the UI becomes unreachable.
  const tlsAddrs = $derived(info.data?.listeners.bound['web-tls'] ?? [])
  const tlsPort = $derived(tlsAddrs.length > 0 ? tlsAddrs[0].slice(tlsAddrs[0].lastIndexOf(':') + 1) : '')
  const onHttp = location.protocol === 'http:'
  const httpsUrl = $derived(
    tlsPort
      ? `https://${location.hostname}${tlsPort === '443' ? '' : ':' + tlsPort}${location.pathname}${location.search}${location.hash}`
      : '',
  )
  const redirectNew = $derived(!!form.draft?.redirectToHttps && !form.saved?.redirectToHttps)

  // Client-side checks block saving; server errors are shown until the next edit or save.
  const local = $derived({
    idle: form.draft ? rangeError(form.draft.sessionIdleMinutes, RANGES.sessionIdleMinutes) : undefined,
    max: form.draft ? rangeError(form.draft.sessionMaxHours, RANGES.sessionMaxHours) : undefined,
    hosts:
      form.draft && form.draft.allowedHosts.length > MAX_ALLOWED_HOSTS
        ? t('system.web.hostsTooMany', { max: MAX_ALLOWED_HOSTS })
        : undefined,
  })
  const invalid = $derived(!!(local.idle || local.max || local.hosts))

  const serverHostError = $derived.by(() => {
    const m = /^web\.allowedHosts\[(\d+)\]$/.exec(form.saveError?.field ?? '')
    if (m) return t('system.web.hostInvalid', { host: form.draft?.allowedHosts[Number(m[1])] ?? '' })
    return form.error('allowedHosts')
  })

  const errors = $derived({
    idle: form.error('sessionIdleMinutes') ?? local.idle,
    max: form.error('sessionMaxHours') ?? local.max,
    hosts: serverHostError ?? local.hosts,
    language: form.error('language'),
    redirect: form.error('redirectToHttps'),
  })
  const general = $derived(form.saveError && !form.saveError.field ? form.errorMessage : undefined)

  const languages = $derived([
    { value: '', label: t('system.web.languageBrowser') },
    { value: 'en', label: 'English' },
    { value: 'de', label: 'Deutsch' },
  ])

  function duration(n: unknown, unitMs: number): string {
    return typeof n === 'number' && Number.isFinite(n) && n > 0 ? formatDuration(n * unitMs) : '–'
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid) return
    const moveToHttps = redirectNew && onHttp && httpsUrl
    if (!(await form.save())) return
    toast.success(t('common.state.saved'))
    if (moveToHttps) location.href = httpsUrl
  }

  const formId = $props.id()
</script>

<Panel title={t('system.web.title')} description={t('system.web.description')}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.web.loadError')}>{form.loadError.message}</Notice>
  {:else if !form.draft}
    <Skeleton height="240px" />
  {:else}
    <form id="web-{formId}" class="stack" onsubmit={save} novalidate>
      {#if general}<Notice tone="fail">{general}</Notice>{/if}

      <div class="cols-2">
        <Field
          label={t('system.web.idle')}
          error={errors.idle}
          help={t('system.web.idleHelp', {
            duration: duration(form.draft.sessionIdleMinutes, 60_000),
            def: duration(form.defaults?.sessionIdleMinutes, 60_000),
          })}
        >
          <Input
            type="number"
            bind:value={form.draft.sessionIdleMinutes}
            min={RANGES.sessionIdleMinutes.min}
            max={RANGES.sessionIdleMinutes.max}
            step={1}
            inputmode="numeric"
            disabled={!session.isAdmin}
          />
        </Field>
        <Field
          label={t('system.web.max')}
          error={errors.max}
          help={t('system.web.maxHelp', {
            duration: duration(form.draft.sessionMaxHours, 3_600_000),
            def: duration(form.defaults?.sessionMaxHours, 3_600_000),
          })}
        >
          <Input
            type="number"
            bind:value={form.draft.sessionMaxHours}
            min={RANGES.sessionMaxHours.min}
            max={RANGES.sessionMaxHours.max}
            step={1}
            inputmode="numeric"
            disabled={!session.isAdmin}
          />
        </Field>
      </div>

      <Field label={t('system.web.hosts')} optional error={errors.hosts} help={t('system.web.hostsHelp')}>
        <Textarea
          bind:value={() => hostsText, setHosts}
          rows={3}
          mono
          placeholder="picache.home.arpa"
          disabled={!session.isAdmin}
        />
      </Field>

      <Field label={t('system.web.language')} error={errors.language} help={t('system.web.languageHelp')}>
        <Select
          options={languages}
          bind:value={() => form.draft?.language ?? '', (v) => form.draft && (form.draft.language = v as WebSettings['language'])}
          disabled={!session.isAdmin}
        />
      </Field>

      <div class="stack-sm">
        <Checkbox
          bind:checked={form.draft.redirectToHttps}
          label={t('system.web.redirect')}
          description={t('system.web.redirectHelp')}
          disabled={!session.isAdmin || (!form.draft.redirectToHttps && info.loaded && tlsAddrs.length === 0)}
        />
        {#if errors.redirect}<p class="small err">{errors.redirect}</p>{/if}
        {#if info.loaded && tlsAddrs.length === 0 && !form.draft.redirectToHttps}
          <p class="small muted">{t('system.web.redirectNoTls')}</p>
        {/if}
        {#if redirectNew && onHttp && httpsUrl}
          <Notice tone="info">{t('system.web.redirectMove', { url: httpsUrl })}</Notice>
        {/if}
      </div>
    </form>
  {/if}
  {#snippet footer()}
    {#if form.draft}
      <div class="row">
        <Button
          type="submit"
          form="web-{formId}"
          variant="primary"
          loading={form.saving}
          disabled={!form.dirty || invalid || !session.isAdmin}
        >
          {t('common.action.save')}
        </Button>
        <Button variant="ghost" disabled={!form.dirty || form.saving} onclick={revert}>
          {t('system.form.discard')}
        </Button>
      </div>
    {/if}
  {/snippet}
</Panel>

<style>
  .err {
    color: var(--danger);
  }
</style>
