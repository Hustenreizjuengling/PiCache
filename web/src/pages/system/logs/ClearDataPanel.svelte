<!--
  @component
  Clear data (admins, only while the host allows destructive actions):
  delete the whole query log or reset the statistics now. Each asks first
  and names what is kept.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { Button, Panel } from '$lib/ui'
  import { clearQueryLog, resetStatistics } from './clear'
</script>

<Panel id="clear" title={t('system.logs.clear.title')} description={t('system.logs.clear.description')} flush>
  <ul class="actions">
    <li>
      <div class="text">
        <p class="name">{t('system.logs.clear.queries')}</p>
        <p class="small muted">{t('system.logs.clear.queriesText')}</p>
      </div>
      <Button variant="danger" icon="trash" onclick={clearQueryLog}>{t('system.logs.clear.queries')}</Button>
    </li>
    <li>
      <div class="text">
        <p class="name">{t('system.logs.clear.stats')}</p>
        <p class="small muted">{t('system.logs.clear.statsText')}</p>
      </div>
      <Button variant="danger" icon="trash" onclick={resetStatistics}>{t('system.logs.clear.stats')}</Button>
    </li>
  </ul>
</Panel>

<style>
  .actions {
    margin: 0;
    padding: 0;
    list-style: none;
    border-top: 1px solid var(--line);
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-4);
    padding: var(--sp-3) var(--sp-4);
  }
  li + li {
    border-top: 1px solid var(--line);
  }
  .text {
    flex: 1 1 320px;
    min-width: 0;
  }
  .name {
    font-weight: 600;
  }
</style>
