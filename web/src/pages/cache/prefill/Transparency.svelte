<!--
  @component
  What the prefill service is and may do: where the sign-in is kept and
  where it is not, that downloads only go through the cache, and every
  host the service talked to since it started (with the reason).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { PrefillHost, PrefillStatus } from '$lib/api'
  import { formatRelative } from '$lib/format'
  import { Panel, Table, type Column } from '$lib/ui'

  let { status }: { status: PrefillStatus | undefined } = $props()

  const columns: Column<PrefillHost>[] = $derived([
    { key: 'host', label: t('cache.prefill.hosts.host'), mono: true, value: (h) => h.host },
    { key: 'protocol', label: t('cache.prefill.hosts.protocol'), mono: true, value: (h) => h.protocol },
    { key: 'purpose', label: t('cache.prefill.hosts.purpose'), format: (h) => t(`cache.prefill.hosts.purposeName.${h.purpose}`) },
    { key: 'lastUsed', label: t('cache.prefill.hosts.lastUsed'), format: (h) => (h.lastUsed ? formatRelative(h.lastUsed) : '–') },
  ])
</script>

<Panel title={t('cache.prefill.trust.title')} description={t('cache.prefill.trust.description')}>
  <div class="stack">
    <ul class="facts">
      <li>{t('cache.prefill.trust.process')}</li>
      <li>{t('cache.prefill.trust.token')}</li>
      <li>{t('cache.prefill.trust.downloads')}</li>
      <li>{t('cache.prefill.trust.revoke')}</li>
      <li>{t('cache.prefill.trust.source')}</li>
    </ul>
    {#if status}
      <Table caption={t('cache.prefill.hosts.title')} rows={status.hosts} key={(h) => h.protocol + '://' + h.host} {columns} compact emptyText={t('cache.prefill.hosts.empty')} />
    {/if}
  </div>
</Panel>

<style>
  .facts {
    margin: 0;
    padding-left: var(--sp-5);
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
