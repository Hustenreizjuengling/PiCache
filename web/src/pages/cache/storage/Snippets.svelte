<!--
  @component
  Ready-to-paste configuration for mounting a storage target outside PiCache
  (fstab, systemd, Docker, Proxmox, credentials file, host-apply command).
  Snippets never contain the NAS password. Admins only.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type StorageSnippets } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { CopyButton, Notice, Skeleton, Tabs, type TabItem } from '$lib/ui'

  let { id }: { id: string } = $props()

  const snippets = resource((signal) => api.storage.snippets(id, { signal }))

  type Key = Exclude<keyof StorageSnippets, 'notes'>
  const ORDER: Key[] = ['fstab', 'systemdMount', 'dockerCompose', 'proxmox', 'credentialsFile', 'hostApply']

  const available = $derived(ORDER.filter((k) => !!snippets.data?.[k]))
  const tabs: TabItem[] = $derived(available.map((k) => ({ id: k, label: t(`cache.snippets.${k}`) })))
  let active = $state('')
  const current = $derived((available as string[]).includes(active) ? (active as Key) : available[0])
</script>

{#if snippets.error && !snippets.data}
  <Notice tone="fail">{errorText(snippets.error)}</Notice>
{:else if !snippets.data}
  <Skeleton height="120px" />
{:else}
  <div class="stack">
    {#if snippets.data.notes?.length}
      <ul class="notes">
        {#each snippets.data.notes as n, i (i)}<li>{n}</li>{/each}
      </ul>
    {/if}
    {#if available.length === 0}
      <p class="muted small">{t('cache.snippets.none')}</p>
    {:else}
      <Tabs label={t('cache.snippets.label')} {tabs} bind:active={() => current ?? '', (v) => (active = v)}>
        {#snippet children(tab)}
          {@const text = snippets.data?.[tab as Key] ?? ''}
          <div class="snippet">
            <p class="muted small">{t(`cache.snippets.${tab as Key}Help`)}</p>
            <div class="code">
              <pre class="mono">{text}</pre>
              <span class="copy"><CopyButton {text} label={t('cache.snippets.copy')} /></span>
            </div>
          </div>
        {/snippet}
      </Tabs>
    {/if}
  </div>
{/if}

<style>
  .notes {
    margin: 0;
    padding-left: var(--sp-5);
    font-size: var(--fs-sm);
    color: var(--text-2);
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
  }
  .snippet {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .code {
    position: relative;
  }
  pre {
    margin: 0;
    padding: var(--sp-3) var(--sp-7) var(--sp-3) var(--sp-3);
    max-height: 320px;
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    font-size: var(--fs-sm);
    line-height: 1.5;
    white-space: pre;
    overflow-wrap: normal;
  }
  .copy {
    position: absolute;
    top: var(--sp-2);
    right: var(--sp-2);
  }
</style>
