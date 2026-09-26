<!--
  @component
  Debug logging for a while: a more detailed level than PICACHE_LOG_LEVEL
  for all components or one, for 1–240 minutes (memory only: it ends by
  itself and with a restart). Shows the active override with its end and
  "End now".
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type LogLevelState, type LogOverride } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatRelative, formatTime } from '$lib/format'
  import { Button, Field, Notice, Panel, Select, toast } from '$lib/ui'
  import NumberInput from '../../dns/shared/NumberInput.svelte'
  import { rangeError, RANGES } from '../forms'
  import { levelRank } from './records'

  interface Props {
    baseLevel: string
    override?: LogOverride
    components: readonly string[]
    onchange: (state: { baseLevel: string; override?: LogOverride }) => void
  }

  let { baseLevel, override, components, onchange }: Props = $props()

  /** Levels more detailed than the base level (the server refuses the others). */
  const levels = $derived((['debug', 'info'] as const).filter((l) => levelRank(l) < levelRank(baseLevel)))

  let level = $state<string>('debug')
  let component = $state('')
  let minutes = $state(30)
  let busy = $state(false)
  let ending = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)

  // "Now" moves on, so an override that ended by itself disappears.
  let now = $state(Date.now())
  $effect(() => {
    const id = setInterval(() => (now = Date.now()), 15_000)
    return () => clearInterval(id)
  })

  const active = $derived(override && Date.parse(override.until) > now ? override : undefined)
  const minutesError = $derived(fieldError(err, 'minutes') ?? rangeError(minutes, RANGES.logLevelMinutes))
  const general = $derived(err && !err.field ? errorText(err) : undefined)

  $effect(() => {
    if (levels.length > 0 && !(levels as readonly string[]).includes(level)) level = levels[0]
  })

  function target(o: LogOverride): string {
    return o.component ? o.component : t('system.applog.debug.all')
  }

  function levelName(l: string): string {
    const k = l.toLowerCase()
    return k === 'debug' || k === 'info' || k === 'warn' || k === 'error' ? t(`system.applog.level.${k}`) : l
  }

  async function start(e: SubmitEvent) {
    e.preventDefault()
    if (busy || rangeError(minutes, RANGES.logLevelMinutes)) return
    busy = true
    err = undefined
    try {
      const st: LogLevelState = await api.system.setLogLevel({
        level: level === 'info' ? 'info' : 'debug',
        component: component || undefined,
        minutes,
      })
      onchange(st)
      toast.success(t('system.applog.debug.started', { time: formatTime(st.override.until) }))
    } catch (x) {
      err = toApiError(x)
    } finally {
      busy = false
    }
  }

  async function end() {
    ending = true
    try {
      await api.system.clearLogLevel()
      onchange({ baseLevel, override: undefined })
      toast.success(t('system.applog.debug.ended'))
    } catch (x) {
      toast.error(x)
    } finally {
      ending = false
    }
  }

  const componentOptions = $derived([
    { value: '', label: t('system.applog.allComponents') },
    ...components.map((c) => ({ value: c, label: c })),
  ])
  const formId = $props.id()
</script>

<Panel id="debug" title={t('system.applog.debug.title')} description={t('system.applog.debug.description')}>
  <div class="stack">
    <p class="small muted">{t('system.applog.debug.base', { level: levelName(baseLevel) })}</p>

    {#if active}
      <Notice tone="info" title={t('system.applog.debug.activeTitle', { level: levelName(active.level) })}>
        {t('system.applog.debug.active', {
          target: target(active),
          time: formatTime(active.until),
          relative: formatRelative(active.until, now),
        })}
        {#snippet actions()}
          <Button size="sm" icon="close" loading={ending} onclick={end}>{t('system.applog.debug.end')}</Button>
        {/snippet}
      </Notice>
    {/if}

    {#if levels.length === 0}
      <p class="small">{t('system.applog.debug.alreadyDebug')}</p>
    {:else}
      <form id="level-{formId}" class="grid" onsubmit={start} novalidate>
        {#if general}<div class="full"><Notice tone="fail">{general}</Notice></div>{/if}
        <Field label={t('system.applog.debug.level')} error={fieldError(err, 'level')}>
          <Select bind:value={level} options={levels.map((l) => ({ value: l, label: levelName(l) }))} />
        </Field>
        <Field label={t('system.applog.component')} error={fieldError(err, 'component')}>
          <Select bind:value={component} options={componentOptions} />
        </Field>
        <Field label={t('system.applog.debug.minutes')} error={minutesError}>
          <NumberInput bind:value={minutes} min={RANGES.logLevelMinutes.min} max={RANGES.logLevelMinutes.max} unit={t('system.applog.debug.minutesUnit')} />
        </Field>
        <div class="submit">
          <Button type="submit" variant="primary" icon="play" loading={busy}>
            {active ? t('system.applog.debug.replace') : t('system.applog.debug.start')}
          </Button>
        </div>
      </form>
    {/if}
  </div>
</Panel>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 200px), 1fr));
    align-items: end;
    gap: var(--sp-3) var(--sp-4);
  }
  .full {
    grid-column: 1 / -1;
  }
  .submit {
    display: flex;
    align-items: flex-end;
  }
</style>
