<!--
  @component
  Local DNS: records PiCache answers itself, conditional forwarders and the
  router resolver status.
  Query: ?tab=records|forwarders&sel=<id>&search=…
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { router } from '$lib/router.svelte'
  import { Tabs } from '$lib/ui'
  import ForwardersTab from './localdns/ForwardersTab.svelte'
  import RecordsTab from './localdns/RecordsTab.svelte'
  import RouterStatus from './localdns/RouterStatus.svelte'

  const tab = $derived(router.param('tab') === 'forwarders' ? 'forwarders' : 'records')

  const tabs = $derived([
    { id: 'records', label: t('dns.localDns.tab.records') },
    { id: 'forwarders', label: t('dns.localDns.tab.forwarders') },
  ])

  function selectTab(id: string) {
    router.setQuery({ tab: id === 'records' ? null : id, sel: null, search: null })
  }
</script>

<div class="page">
  <Tabs {tabs} active={tab} label={t('common.nav.localDns')} onchange={selectTab}>
    {#snippet children(active)}
      <div class="tab">
        {#if active === 'forwarders'}
          <ForwardersTab />
        {:else}
          <RecordsTab />
        {/if}
      </div>
    {/snippet}
  </Tabs>

  <RouterStatus />
</div>

<style>
  .tab {
    padding-top: var(--sp-4);
  }
</style>
