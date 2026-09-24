<!--
  @component
  Add or edit a custom download service: a name, a description and the host
  names PiCache should cache for it (exact names or *.example.com).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, type LanCacheService } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, Dialog, Field, Input, Notice, Textarea, toast } from '$lib/ui'
  import { parseList } from '../shared/util'

  interface Props {
    open?: boolean
    /** The service to edit (with all domains); undefined adds a new one. */
    service?: LanCacheService
    onsaved: (s: LanCacheService) => void
  }

  let { open = $bindable(false), service, onsaved }: Props = $props()

  const MAX_NAME = 64
  const MAX_DESC = 512

  let name = $state('')
  let description = $state('')
  let domains = $state('')
  let saving = $state(false)
  let error = $state<unknown>(undefined)
  let attempted = $state(false)

  // Fill the form each time the dialog opens (later updates of `service` do
  // not overwrite what is being typed).
  $effect(() => {
    if (!open) return
    untrack(() => {
      name = service?.name ?? ''
      description = service?.description ?? ''
      domains = (service?.domains ?? []).join('\n')
      error = undefined
      attempted = false
    })
  })

  const list = $derived(parseList(domains))
  const nameMissing = $derived(attempted && !name.trim())
  const domainsMissing = $derived(attempted && list.length === 0)
  const otherError = $derived(
    error && !fieldError(error, 'name') && !fieldError(error, 'description') && !fieldError(error, 'domains')
      ? errorText(error)
      : undefined,
  )

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    attempted = true
    if (!name.trim() || list.length === 0) return
    saving = true
    error = undefined
    try {
      const input = { name: name.trim(), description: description.trim(), domains: list }
      const saved = service ? await api.lancache.updateService(service.id, input) : await api.lancache.createService(input)
      toast.success(service ? t('cache.services.customSaved', { name: saved.name }) : t('cache.services.customAdded', { name: saved.name }))
      open = false
      onsaved(saved)
    } catch (err) {
      error = err
    } finally {
      saving = false
    }
  }
</script>

<Dialog
  bind:open
  dismissible={!saving}
  title={service ? t('cache.services.editCustomTitle', { name: service.name }) : t('cache.services.addCustomTitle')}
>
  <form id="custom-service" class="stack" onsubmit={submit} novalidate>
    <p class="muted small">{t('cache.services.customIntro')}</p>
    {#if otherError}<Notice tone="fail">{otherError}</Notice>{/if}
    <Field
      label={t('common.label.name')}
      required
      error={nameMissing ? t('common.field.required') : fieldError(error, 'name')}
    >
      <Input bind:value={name} maxlength={MAX_NAME} placeholder={t('cache.services.customNamePlaceholder')} />
    </Field>
    <Field label={t('cache.services.description')} optional error={fieldError(error, 'description')}>
      <Input bind:value={description} maxlength={MAX_DESC} />
    </Field>
    <Field
      label={t('cache.services.domains')}
      required
      help={t('cache.services.domainsHelp')}
      error={domainsMissing ? t('cache.services.domainsRequired') : fieldError(error, 'domains')}
    >
      <Textarea bind:value={domains} rows={8} mono placeholder={'downloads.example.com\n*.cdn.example.net'} />
    </Field>
  </form>
  {#snippet actions()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button variant="primary" type="submit" form="custom-service" loading={saving}>
      {service ? t('common.action.save') : t('cache.services.addCustom')}
    </Button>
  {/snippet}
</Dialog>
