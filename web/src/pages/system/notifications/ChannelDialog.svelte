<!--
  @component
  Add or edit a notification channel: the type (webhook, ntfy, Gotify) with
  help for its address and secret, the minimum severity and the events it
  gets. The secret is write-only: it is never shown, only sent when typed
  (replace) or removed; leaving the field empty keeps the stored one.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import {
    api,
    type ApiError,
    type NotifyChannel,
    type NotifyChannelInput,
    type NotifyEvent,
    type NotifyKind,
    type NotifySeverity,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, Checkbox, Chip, Dialog, Field, Input, Notice, Select, toast } from '$lib/ui'
  import { charCount } from '../forms'
  import {
    KINDS,
    MAX_NAME,
    MAX_URL,
    SEVERITIES,
    TEST_EVENT,
    eventText,
    hasControlChars,
    kindLabel,
    passes,
    severityLabel,
    severityTone,
    urlProblem,
  } from './channels'

  interface Props {
    open?: boolean
    /** The channel to edit; undefined adds a new one. */
    channel?: NotifyChannel
    /** GET /notifications/events (undefined while loading or when it failed). */
    catalog: readonly NotifyEvent[] | undefined
    catalogError?: ApiError
    onsaved: (channel: NotifyChannel, created: boolean) => void
  }

  let { open = $bindable(false), channel, catalog, catalogError, onsaved }: Props = $props()

  let kind = $state<NotifyKind>('ntfy')
  let name = $state('')
  let url = $state('')
  let secret = $state('')
  let removeSecret = $state(false)
  let enabled = $state(true)
  let minSeverity = $state<NotifySeverity>('warning')
  let events = $state<string[]>([])

  let saving = $state(false)
  let error = $state<unknown>(undefined)
  let attempted = $state(false)

  // Fill the form each time the dialog opens (later list refreshes do not
  // overwrite what is being typed).
  $effect(() => {
    if (!open) return
    untrack(() => {
      const c = channel
      kind = c?.kind ?? 'ntfy'
      name = c?.name ?? ''
      url = c?.url ?? ''
      secret = ''
      removeSecret = false
      enabled = c?.enabled ?? true
      minSeverity = c?.minSeverity ?? 'warning'
      events = [...(c?.events ?? [])]
      error = undefined
      attempted = false
    })
  })

  /** The stored secret still applies: same type as when it was saved. */
  const keepable = $derived(!!channel?.hasSecret && channel.kind === kind)
  /** The stored secret is dropped because the type changed. */
  const kindChanged = $derived(!!channel?.hasSecret && channel.kind !== kind)
  /** What the save sends: undefined keeps, '' removes, a value replaces. */
  const secretOut = $derived.by((): string | undefined => {
    if (keepable && removeSecret) return '' // the field is disabled then; ignore what was typed before
    if (secret) return secret
    if (kindChanged) return ''
    return undefined
  })
  const willHaveSecret = $derived(secretOut === undefined ? keepable : secretOut !== '')

  // ---- events: the catalogue without the test event (always delivered), plus
  // selected keys this server did not list (so they can be unselected).
  const choices = $derived.by((): NotifyEvent[] => {
    const list = (catalog ?? []).filter((e) => e.key !== TEST_EVENT)
    const extra = events.filter((k) => !list.some((e) => e.key === k))
    return [...list, ...extra.map((key): NotifyEvent => ({ key, severity: 'info', title: key, description: '' }))]
  })

  function toggleEvent(key: string, on: boolean) {
    events = on ? [...events.filter((k) => k !== key), key] : events.filter((k) => k !== key)
  }

  // ---- checks (the server validates again)

  const problems = $derived.by(() => {
    const p: Record<string, string | undefined> = {}
    const n = name.trim()
    if (!n) p.name = t('common.field.required')
    else if (charCount(n) > MAX_NAME) p.name = t('system.notifications.form.nameTooLong', { max: MAX_NAME })
    else if (hasControlChars(n)) p.name = t('system.notifications.form.nameControl')
    const u = urlProblem(url)
    if (u === 'required') p.url = t('common.field.required')
    else if (u === 'tooLong') p.url = t('system.notifications.form.urlTooLong', { max: MAX_URL })
    else if (u === 'scheme') p.url = t('system.notifications.form.urlScheme')
    else if (u === 'invalid') p.url = t('system.notifications.form.urlInvalid')
    if (secretOut && hasControlChars(secretOut)) p.secret = t('system.notifications.form.secretControl')
    else if (kind === 'gotify' && !willHaveSecret) p.secret = t('system.notifications.form.secretRequired')
    return p
  })
  const valid = $derived(Object.values(problems).every((v) => !v))

  const FIELDS = ['name', 'kind', 'url', 'secret', 'enabled', 'minSeverity', 'events']

  /** Client problem (after the first submit) or the server's message for this field. */
  function err(field: string): string | undefined {
    return (attempted ? problems[field] : undefined) ?? fieldError(error, field)
  }

  const otherError = $derived(error && !FIELDS.some((f) => fieldError(error, f)) ? errorText(error) : undefined)

  const plainHttp = $derived(/^http:\/\//i.test(url.trim()) && willHaveSecret)

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    attempted = true
    if (!valid) return
    saving = true
    error = undefined
    try {
      const input: NotifyChannelInput = {
        name: name.trim(),
        kind,
        url: url.trim(),
        enabled,
        minSeverity,
        events: [...events],
      }
      // Write-only: absent keeps the stored secret, '' removes it.
      if (secretOut !== undefined) input.secret = secretOut
      const saved = channel
        ? await api.notifications.channels.update(channel.id, input)
        : await api.notifications.channels.create(input)
      secret = ''
      toast.success(
        channel
          ? t('system.notifications.form.saved', { name: saved.name })
          : t('system.notifications.form.added', { name: saved.name }),
      )
      open = false
      onsaved(saved, !channel)
    } catch (err) {
      error = err
    } finally {
      saving = false
    }
  }

  function closed() {
    secret = '' // never keep a typed token around
  }

  const severityOptions = $derived(SEVERITIES.map((s) => ({ value: s, label: t(`system.notifications.min.${s}`) })))

  const URL_PLACEHOLDER: Record<NotifyKind, string> = {
    webhook: 'http://homeassistant.local:8123/api/webhook/…',
    ntfy: 'https://ntfy.sh/…',
    gotify: 'https://gotify.lan',
  }

  const formId = $props.id()
</script>

<Dialog
  bind:open
  size="md"
  dismissible={!saving}
  title={channel ? t('system.notifications.form.editTitle', { name: channel.name }) : t('system.notifications.form.addTitle')}
  onclose={closed}
>
  <form id="channel-{formId}" class="stack" onsubmit={submit} novalidate>
    {#if otherError}<Notice tone="fail">{otherError}</Notice>{/if}

    <fieldset>
      <legend>{t('system.notifications.form.kind')}</legend>
      <div class="kinds">
        {#each KINDS as k (k)}
          <label class={['choice', kind === k && 'checked']}>
            <input type="radio" name="kind-{formId}" value={k} checked={kind === k} onchange={() => (kind = k)} />
            <span class="choice-text">
              <span class="choice-title">{kindLabel(k)}</span>
              <span class="choice-desc">{t(`system.notifications.kindText.${k}`)}</span>
            </span>
          </label>
        {/each}
      </div>
      {#if err('kind')}<p class="field-error">{err('kind')}</p>{/if}
    </fieldset>

    <Field label={t('common.label.name')} required error={err('name')} help={t('system.notifications.form.nameHelp')}>
      <Input bind:value={name} maxlength={MAX_NAME} autocomplete="off" placeholder={t('system.notifications.form.namePlaceholder')} />
    </Field>

    <Field
      label={t(`system.notifications.form.url.${kind}`)}
      required
      error={err('url')}
      help={t(`system.notifications.form.urlHelp.${kind}`)}
    >
      <Input
        bind:value={url}
        type="url"
        mono
        maxlength={MAX_URL}
        autocomplete="off"
        inputmode="url"
        placeholder={URL_PLACEHOLDER[kind]}
      />
    </Field>

    <div class="stack-sm">
      <Field
        label={t(`system.notifications.secret.${kind}`)}
        required={kind === 'gotify'}
        optional={kind !== 'gotify'}
        error={err('secret')}
        help={`${t(`system.notifications.form.secretHelp.${kind}`)} ${t('system.notifications.form.secretSealed')}`}
      >
        <Input
          type="password"
          bind:value={secret}
          autocomplete="new-password"
          disabled={keepable && removeSecret}
          placeholder={keepable && !removeSecret ? t('system.notifications.form.secretKeep') : undefined}
        />
      </Field>
      {#if keepable && kind !== 'gotify'}
        <Checkbox
          bind:checked={removeSecret}
          label={kind === 'webhook' ? t('system.notifications.form.removeHeader') : t('system.notifications.form.removeToken')}
        />
      {/if}
      {#if kindChanged}
        <p class="small muted">{t('system.notifications.form.secretKindChanged')}</p>
      {/if}
      {#if plainHttp}
        <Notice tone="warn">{t('system.notifications.form.plainHttp')}</Notice>
      {/if}
    </div>

    <Checkbox
      bind:checked={enabled}
      label={t('system.notifications.form.enabled')}
      description={t('system.notifications.form.enabledHelp')}
    />

    <Field label={t('system.notifications.form.minSeverity')} error={err('minSeverity')} help={t('system.notifications.form.minSeverityHelp')}>
      <Select
        options={severityOptions}
        bind:value={() => minSeverity, (v) => (minSeverity = SEVERITIES.includes(v as NotifySeverity) ? (v as NotifySeverity) : 'warning')}
      />
    </Field>

    <fieldset class="events" aria-describedby="events-help-{formId}">
      <legend>{t('system.notifications.form.events')}</legend>
      {#if catalogError && !catalog}
        <Notice tone="warn">{t('system.notifications.form.eventsError')}</Notice>
      {/if}
      {#if choices.length > 0}
        <div class="summary">
          <span class="small muted" id="events-help-{formId}" aria-live="polite">
            {events.length === 0
              ? t('system.notifications.form.eventsHelp')
              : tn('system.notifications.form.eventsSome', events.length)}
          </span>
          {#if events.length > 0}
            <Button size="sm" variant="ghost" onclick={() => (events = [])}>{t('system.notifications.form.eventsClear')}</Button>
          {/if}
        </div>
        <ul class="event-list">
          {#each choices as ev (ev.key)}
            {@const text = eventText(ev.key, catalog)}
            {@const below = !passes(minSeverity, ev.severity)}
            <li class={{ below }}>
              <Checkbox
                checked={events.includes(ev.key)}
                label={text.title}
                description={below ? [text.description, t('system.notifications.form.eventBelow')].filter(Boolean).join(' ') : text.description}
                onchange={(on) => toggleEvent(ev.key, on)}
              />
              <Chip size="sm" tone={severityTone(ev.severity)} label={severityLabel(ev.severity)} />
            </li>
          {/each}
        </ul>
      {/if}
      {#if err('events')}<p class="field-error">{err('events')}</p>{/if}
    </fieldset>
  </form>

  {#snippet actions()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button variant="primary" type="submit" form="channel-{formId}" icon={channel ? undefined : 'plus'} loading={saving}>
      {channel ? t('common.action.save') : t('system.notifications.form.add')}
    </Button>
  {/snippet}
</Dialog>

<style>
  fieldset {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    border: 0;
    min-width: 0;
  }
  legend {
    margin-bottom: var(--sp-1);
    padding: 0;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .kinds {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: var(--sp-2);
  }
  .choice {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    cursor: pointer;
    min-width: 0;
  }
  .choice:hover {
    background: var(--surface-2);
  }
  .choice.checked {
    border-color: var(--text);
    box-shadow: inset 0 0 0 1px var(--text);
  }
  .choice input {
    margin-top: 3px;
    accent-color: var(--text);
  }
  .choice:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .choice-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .choice-title {
    font-weight: 600;
  }
  .choice-desc {
    font-size: var(--fs-xs);
    color: var(--text-2);
  }
  .summary {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-1) var(--sp-2);
    min-height: var(--control-h-sm);
  }
  .event-list {
    margin: 0;
    padding: 0;
    list-style: none;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    max-height: 18rem;
    overflow: auto;
    overscroll-behavior: contain;
  }
  .event-list li {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-3);
    padding: var(--sp-2) var(--sp-3);
    border-bottom: 1px solid var(--line);
  }
  .event-list li:last-child {
    border-bottom: 0;
  }
  .event-list li :global(.cb) {
    flex: 1;
  }
  .event-list li :global(.chip) {
    flex: none;
    margin-top: 1px;
  }
  .below :global(.cb > .text > span:first-child) {
    color: var(--text-2);
  }
  .field-error {
    font-size: var(--fs-sm);
    color: var(--danger);
  }
</style>
