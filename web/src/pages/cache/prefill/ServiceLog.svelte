<!--
  @component
  The prefill service's own log: its last lines from the file in its state
  directory (kept across restarts and crashes), loaded on request, so a
  problem can be seen without a shell on the host.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api } from '$lib/api'
  import { Button, CopyButton, EmptyState, Panel, toast } from '$lib/ui'

  let lines = $state<string[] | undefined>(undefined)
  let loading = $state(false)

  async function load() {
    loading = true
    try {
      lines = (await api.prefill.log(300)).lines
    } catch (err) {
      toast.error(err)
    } finally {
      loading = false
    }
  }
</script>

<Panel title={t('cache.prefill.log.title')} description={t('cache.prefill.log.description')}>
  {#snippet actions()}
    {#if lines?.length}
      <CopyButton text={lines.join('\n')} label={t('cache.prefill.log.copy')} />
    {/if}
    <Button icon="refresh" {loading} onclick={load}>{lines ? t('cache.prefill.log.refresh') : t('cache.prefill.log.show')}</Button>
  {/snippet}
  {#if lines}
    {#if lines.length === 0}
      <EmptyState compact icon="list" title={t('cache.prefill.log.empty')} />
    {:else}
      <pre class="log">{lines.join('\n')}</pre>
    {/if}
  {/if}
</Panel>

<style>
  .log {
    margin: 0;
    max-height: 360px;
    overflow: auto;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    line-height: 1.5;
    white-space: pre-wrap;
    word-break: break-all;
    color: var(--text-2);
  }
</style>
