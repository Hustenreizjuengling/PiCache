<!--
  @component
  Settings of the prefill service for every store: the time window of
  scheduled jobs (the host's time zone; a job stops at the window's end and
  the next window continues), the download rate and parallel downloads.
-->
<script lang="ts">
  import { i18n, t } from '$i18n/index.svelte'
  import { api, type PrefillSettings, type PrefillStatus } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Button, Checkbox, Field, Input, Panel, Toggle, toast } from '$lib/ui'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  let draft = $state<PrefillSettings | undefined>(undefined)
  const saved = $derived(status.settings)
  const s = $derived(draft ?? saved)
  const dirty = $derived(draft !== undefined && JSON.stringify(draft) !== JSON.stringify(saved))
  const readOnly = $derived(!session.isAdmin)

  function edit(f: (d: PrefillSettings) => void) {
    const d: PrefillSettings = structuredClone($state.snapshot(draft ?? saved))
    f(d)
    draft = d
  }

  // Monday first; the API counts from Sunday (0).
  const DAYS = [1, 2, 3, 4, 5, 6, 0]
  const dayName = $derived((d: number) =>
    new Intl.DateTimeFormat(i18n.tag, { weekday: 'short', timeZone: 'UTC' }).format(new Date(Date.UTC(2026, 9, 18 + d))),
  )

  function toggle<T>(list: T[], v: T, on: boolean): T[] {
    return on ? [...new Set([...list, v])] : list.filter((x) => x !== v)
  }

  let saving = $state(false)
  async function save() {
    if (!draft) return
    saving = true
    try {
      await api.prefill.saveSettings(draft)
      draft = undefined
      toast.success(t('cache.prefill.settings.saved'))
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      saving = false
    }
  }
</script>

<Panel title={t('cache.prefill.settings.title')} description={t('cache.prefill.settings.description')}>
  <div class="stack">
    <Toggle
      id="prefill-schedule"
      label={t('cache.prefill.settings.schedule')}
      description={t('cache.prefill.settings.scheduleHelp')}
      checked={s.schedule.enabled}
      disabled={readOnly}
      onchange={(v) => edit((d) => (d.schedule.enabled = v))}
    />
    <div class="grid" class:off={!s.schedule.enabled}>
      <Field label={t('cache.prefill.settings.start')}>
        <Input type="time" value={s.schedule.start} disabled={readOnly} oninput={(e) => edit((d) => (d.schedule.start = (e.currentTarget as HTMLInputElement).value))} />
      </Field>
      <Field label={t('cache.prefill.settings.end')} help={t('cache.prefill.settings.endHelp')}>
        <Input type="time" value={s.schedule.end} disabled={readOnly} oninput={(e) => edit((d) => (d.schedule.end = (e.currentTarget as HTMLInputElement).value))} />
      </Field>
    </div>
    <fieldset class="checks" class:off={!s.schedule.enabled}>
      <legend>{t('cache.prefill.settings.days')}</legend>
      {#each DAYS as day (day)}
        <Checkbox
          label={dayName(day)}
          checked={s.schedule.days.includes(day)}
          disabled={readOnly}
          onchange={(v) => edit((d) => (d.schedule.days = toggle(d.schedule.days, day, v)))}
        />
      {/each}
    </fieldset>

    <div class="grid">
      <Field label={t('cache.prefill.settings.maxMbps')} help={t('cache.prefill.settings.maxMbpsHelp')}>
        <Input type="number" min="0" max="100000" value={s.maxMbps} disabled={readOnly}
          oninput={(e) => edit((d) => (d.maxMbps = Math.max(0, Math.round(Number((e.currentTarget as HTMLInputElement).value) || 0))))} />
      </Field>
      <Field label={t('cache.prefill.settings.connections')} help={t('cache.prefill.settings.connectionsHelp')}>
        <Input type="number" min="1" max="32" value={s.connections} disabled={readOnly}
          oninput={(e) => edit((d) => (d.connections = Math.round(Number((e.currentTarget as HTMLInputElement).value) || 1)))} />
      </Field>
    </div>

  </div>
  {#if dirty}
    <div class="save">
      <Button variant="ghost" onclick={() => (draft = undefined)}>{t('cache.prefill.settings.discard')}</Button>
      <Button variant="primary" icon="check" loading={saving} onclick={save}>{t('cache.prefill.settings.save')}</Button>
    </div>
  {/if}
</Panel>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    gap: var(--sp-4);
  }
  .off {
    opacity: 0.6;
  }
  .checks {
    border: 0;
    margin: 0;
    padding: 0;
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2) var(--sp-5);
  }
  .checks legend {
    font-weight: 600;
    margin-bottom: var(--sp-2);
    padding: 0;
  }
  .save {
    display: flex;
    justify-content: flex-end;
    gap: var(--sp-2);
    margin-top: var(--sp-4);
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
</style>
