<!--
  @component
  Starts a storage speed test (POST /storage/targets/{id}/benchmark): the
  size (64 MiB quick, 256 MiB default, 1 GiB thorough), what the test does
  and that it loads the storage for up to two minutes. Opened from a target
  (`fixed`) or from the page, where the storage is chosen among the online
  ones. Refusals stay in the dialog with the server's message: 400 with field
  "sizeMiB" under the sizes, others (400 not enough free space, 409 already
  running or not available, 404, 503) above. Mount it only while open
  ({#if}), so it starts fresh.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, type MessageKey } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type BenchmarkRun, type BenchmarkSize, type StorageTargetWithStatus } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatBytes } from '$lib/format'
  import { Button, Dialog, Field, Icon, Notice, Select, toast } from '$lib/ui'
  import { DEFAULT_SIZE, SIZES, formatMiB, neededBytes } from './speed'

  interface Props {
    open?: boolean
    /** Online storage targets that can be tested. */
    targets: StorageTargetWithStatus[]
    /** The target to test (preselected when the storage can be chosen). */
    targetId: string
    /** Opened from a target: the storage cannot be changed. */
    fixed?: boolean
    /** A test is running (e.g. started by another admin meanwhile): starting is refused. */
    running?: boolean
    onstarted: (run: BenchmarkRun) => void
    /**
     * The server refused because of the current state (409: a test is already
     * running or the storage is not available; 404: the storage was deleted;
     * 503: shutting down): reload it.
     */
    onrefused?: () => void
  }

  let { open = $bindable(true), targets, targetId, fixed = false, running = false, onstarted, onrefused }: Props = $props()

  /** Name and description of each size. */
  const LABELS: Record<BenchmarkSize, readonly [MessageKey, MessageKey]> = {
    64: ['cache.speed.sizeQuick', 'cache.speed.sizeQuickText'],
    256: ['cache.speed.sizeStandard', 'cache.speed.sizeStandardText'],
    1024: ['cache.speed.sizeThorough', 'cache.speed.sizeThoroughText'],
  }

  let chosen = $state(untrack(() => targetId || targets[0]?.id || ''))
  let picked = $state<BenchmarkSize>(DEFAULT_SIZE)
  let busy = $state(false)
  let apiErr = $state.raw<ApiError | null>(null)

  const target = $derived(targets.find((x) => x.id === chosen))
  /** Free space as last checked (undefined when unknown: the server checks anyway). */
  const free = $derived(target && target.status.totalBytes > 0 ? target.status.freeBytes : undefined)
  const fits = (mib: number) => free === undefined || free >= neededBytes(mib)
  /** The picked size, or the largest one that fits (larger sizes need more space). */
  const size = $derived(fits(picked) ? picked : (SIZES.filter(fits).at(-1) ?? picked))
  const noneFits = $derived(!fits(SIZES[0]))

  const sizeError = $derived(fieldError(apiErr, 'sizeMiB'))
  const general = $derived(apiErr && !sizeError ? errorText(apiErr) : '')

  const title = $derived(
    target && (fixed || targets.length === 1) ? t('cache.speed.dialogTitle', { name: target.name }) : t('cache.speed.dialogTitleAny'),
  )

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    if (!target || noneFits || busy || running) return
    busy = true
    apiErr = null
    try {
      const run = await api.storage.benchmark(target.id, size)
      toast.success(t('cache.speed.started'))
      open = false
      onstarted(run)
    } catch (err) {
      const ae = toApiError(err)
      apiErr = ae
      if (ae.code === 'conflict' || ae.code === 'not_found' || ae.code === 'unavailable') onrefused?.()
    } finally {
      busy = false
    }
  }

  const formId = $props.id()
</script>

<Dialog bind:open {title} size="md" dismissible={!busy}>
  <form id="speed-{formId}" class="stack" onsubmit={submit} novalidate>
    {#if general}<Notice tone="fail">{general}</Notice>{/if}

    {#if !fixed && targets.length > 1}
      <Field label={t('cache.speed.storage')}>
        <Select
          bind:value={chosen}
          disabled={busy}
          options={targets.map((x) => ({ value: x.id, label: x.name }))}
          onchange={() => (apiErr = null)}
        />
      </Field>
    {/if}

    <fieldset aria-describedby={sizeError ? `speed-${formId}-size-error` : undefined}>
      <legend>{t('cache.speed.size')}</legend>
      {#each SIZES as s (s)}
        {@const ok = fits(s)}
        {@const [label, text] = LABELS[s]}
        <label class={['choice', size === s && ok && 'checked', !ok && 'disabled']}>
          <input
            type="radio"
            name="speed-size-{formId}"
            value={s}
            checked={size === s && ok}
            disabled={!ok || busy}
            onchange={() => {
              picked = s
              apiErr = null
            }}
          />
          <span class="choice-text">
            <span class="choice-title">{formatMiB(s)} · {t(label)}</span>
            <span class="choice-desc">{ok ? t(text) : t('cache.speed.sizeNoSpace', { size: formatMiB(s) })}</span>
          </span>
        </label>
      {/each}
      {#if sizeError}
        <p class="error" id="speed-{formId}-size-error"><Icon name="alert" size={16} />{sizeError}</p>
      {/if}
    </fieldset>

    {#if noneFits}
      <Notice tone="warn" title={t('cache.speed.noSpaceTitle')}>{t('cache.speed.noSpace', { free: formatBytes(free) })}</Notice>
    {:else}
      <p class="small muted">
        {free !== undefined
          ? t('cache.speed.explain', { size: formatMiB(size), free: formatBytes(free) })
          : t('cache.speed.explainNoFree', { size: formatMiB(size) })}
      </p>
      <Notice tone="warn">{t('cache.speed.load')}</Notice>
    {/if}
  </form>

  {#snippet actions()}
    <Button variant="ghost" disabled={busy} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="speed-{formId}" variant="primary" icon="overview" loading={busy} disabled={!target || noneFits || running}>
      {t('cache.speed.start')}
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
    margin-bottom: var(--sp-2);
    padding: 0;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .choice {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    cursor: pointer;
  }
  .choice:hover:not(.disabled) {
    background: var(--surface-2);
  }
  .choice.checked {
    border-color: var(--text);
    box-shadow: inset 0 0 0 1px var(--text);
  }
  .choice.disabled {
    cursor: not-allowed;
    color: var(--text-3);
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
    font-size: var(--fs-sm);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .disabled .choice-desc {
    color: var(--warning);
  }
  .error {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    font-size: var(--fs-sm);
    color: var(--danger);
  }
  .error :global(.icon) {
    margin-top: 1px;
  }
</style>
