<!--
  @component
  Shown after saving a more private setting (query log off, client addresses
  anonymised, domains hidden, statistics off): the data recorded before stays
  until it expires, so this offers to delete it now. The choices are ticked
  as `offer` suggests; nothing is deleted without "Delete selected data".
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { appStatus } from '$lib/status.svelte'
  import { Button, Checkbox, Dialog, Notice, toast } from '$lib/ui'

  interface Props {
    open?: boolean
    offer: { queries: boolean; stats: boolean }
  }

  let { open = $bindable(false), offer }: Props = $props()

  let queries = $state(false)
  let stats = $state(false)
  let busy = $state(false)
  let error = $state('')

  $effect(() => {
    if (!open) return
    untrack(() => {
      queries = offer.queries
      stats = offer.stats
      error = ''
    })
  })

  async function run() {
    busy = true
    error = ''
    try {
      if (queries) {
        const r = await api.logs.clear()
        toast.success(tn('system.logs.clear.queriesDone', r.deleted))
        queries = false // done: a retry after a later failure does not repeat it
      }
      if (stats) {
        const r = await api.stats.reset()
        toast.success(tn('system.logs.clear.statsDone', r.deleted))
        stats = false
        void appStatus.strip.refresh()
      }
      open = false
    } catch (err) {
      error = errorText(err)
    } finally {
      busy = false
    }
  }
</script>

<Dialog bind:open title={t('system.logs.offer.title')} size="sm" dismissible={!busy}>
  <div class="stack">
    <p>{t('system.logs.offer.text')}</p>
    <div class="stack-sm">
      <Checkbox bind:checked={queries} label={t('system.logs.offer.queries')} description={t('system.logs.offer.queriesHelp')} disabled={busy} />
      <Checkbox bind:checked={stats} label={t('system.logs.offer.stats')} description={t('system.logs.offer.statsHelp')} disabled={busy} />
    </div>
    {#if error}<Notice tone="fail">{error}</Notice>{/if}
  </div>
  {#snippet actions()}
    <Button variant="ghost" disabled={busy} onclick={() => (open = false)}>{t('system.logs.offer.keep')}</Button>
    <Button variant="danger" loading={busy} disabled={!queries && !stats} onclick={run}>{t('system.logs.offer.confirm')}</Button>
  {/snippet}
</Dialog>
