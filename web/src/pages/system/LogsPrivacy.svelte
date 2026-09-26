<!--
  @component
  Logs & privacy (settings.logs): the privacy level with its four switches,
  ignored domains, counting only address queries, the write interval, the
  retention of each log and the size cap of logs.db, and "Clear data"
  (clear the query log, reset the statistics; hidden unless destructive
  actions are allowed). One Save for the whole page; after saving a more
  private setting it offers to delete what was recorded before.
  Query: ?section=privacy|recording|retention|clear (scrolls there)
-->
<script lang="ts">
  import { tick, untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { LogsSettings } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Notice, Skeleton, toast } from '$lib/ui'
  import { asciiDomain, sameList } from '../dns/shared/input'
  import { LOG_NUMBERS, MAX_IGNORED_DOMAINS, rangeError, RANGES } from './forms'
  import ClearDataPanel from './logs/ClearDataPanel.svelte'
  import OfferClearDialog from './logs/OfferClearDialog.svelte'
  import { levelOf, offerAfter, type Switches } from './logs/privacy'
  import PrivacyPanel from './logs/PrivacyPanel.svelte'
  import RecordingPanel from './logs/RecordingPanel.svelte'
  import RetentionPanel from './logs/RetentionPanel.svelte'

  const form = settingsForm('logs')

  let root: HTMLElement | undefined = $state()
  /** "Custom" chosen in the level selector; follows the saved level on load. */
  let custom = $state(false)
  let offer = $state<{ queries: boolean; stats: boolean }>({ queries: false, stats: false })
  let offerOpen = $state(false)

  $effect(() => {
    const saved = form.saved
    if (saved) untrack(() => (custom = levelOf(saved) === 'custom'))
  })

  const d = $derived(form.draft)
  const invalid = $derived(
    !!d && (LOG_NUMBERS.some((k) => rangeError(d[k], RANGES[k])) || d.ignoredDomains.length > MAX_IGNORED_DOMAINS),
  )
  const saveError = $derived(form.errorMessage)

  function switches(s: LogsSettings): Switches {
    return {
      queryLogEnabled: s.queryLogEnabled,
      anonymizeClientIps: s.anonymizeClientIps,
      hideDomains: s.hideDomains,
      statsEnabled: s.statsEnabled,
    }
  }

  async function save() {
    const draft = form.draft
    const before = form.saved
    if (!draft || !before || invalid) return
    // Internationalised names are stored as ASCII (punycode), like the server expects.
    const ascii = [...new Set(draft.ignoredDomains.map(asciiDomain).filter(Boolean))]
    if (!sameList(ascii, draft.ignoredDomains)) draft.ignoredDomains = ascii
    const prev = switches(before)
    if (!(await form.save())) {
      await tick()
      root?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
      return
    }
    toast.success(t('common.state.saved'))
    const next = offerAfter(prev, switches(form.saved!))
    if (next && session.canDestroy) {
      offer = next
      offerOpen = true
    }
  }

  const SECTIONS = ['privacy', 'recording', 'retention', 'clear']
  const section = $derived(router.param('section'))
  $effect(() => {
    const s = section
    if (!form.draft || !SECTIONS.includes(s)) return
    untrack(() => void tick().then(() => document.getElementById(s)?.scrollIntoView({ block: 'start' })))
  })
</script>

<div class="page" bind:this={root}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.logs.loadError')}>
      {errorText(form.loadError)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => form.load()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !form.draft}
    <Skeleton height="240px" />
    <Skeleton height="180px" />
  {:else}
    {#if !session.canOperate}<Notice>{t('common.state.readOnly')}</Notice>{/if}

    <fieldset class="sections" disabled={!session.isAdmin}>
      <PrivacyPanel {form} bind:custom />
      <RecordingPanel {form} />
      <RetentionPanel {form} />
    </fieldset>

    {#if session.canDestroy}
      <ClearDataPanel />
    {/if}

    {#if form.dirty || saveError}
      <div class="savebar" role="region" aria-label={t('system.logs.saveBar')}>
        <Button variant="primary" loading={form.saving} disabled={!session.isAdmin || !form.dirty || invalid} onclick={save}>
          {t('common.action.save')}
        </Button>
        <Button variant="ghost" disabled={form.saving || !form.dirty} onclick={() => form.revert()}>{t('system.form.discard')}</Button>
        <div class="msg">
          {#if saveError}
            <p class="err">{saveError}</p>
          {:else}
            <p>{t('system.logs.unsaved')}</p>
          {/if}
        </div>
      </div>
    {/if}
  {/if}
</div>

<OfferClearDialog bind:open={offerOpen} {offer} />

<style>
  .sections {
    display: flex;
    flex-direction: column;
    gap: var(--sp-5);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .page :global(section[id]) {
    scroll-margin-top: var(--sp-4);
  }
  .savebar {
    position: sticky;
    bottom: 0;
    z-index: 5;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
    box-shadow: var(--shadow-float);
  }
  .msg {
    flex: 1 1 240px;
    min-width: 0;
    font-size: var(--fs-sm);
  }
  .err {
    color: var(--danger);
    overflow-wrap: anywhere;
  }
</style>
