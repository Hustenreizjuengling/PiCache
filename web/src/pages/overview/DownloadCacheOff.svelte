<!--
  @component
  Shown instead of the cache band while the download cache is off: what it does and
  how to turn it on.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { Button } from '../../lib/ui'
  import { links } from './links'
</script>

<div class="off">
  <div class="text">
    <p class="lead">{t('overview.off.title')}</p>
    <p class="muted">{t('overview.off.text')}</p>
    <ol>
      <li>{t('overview.off.step1')}</li>
      <li>{t('overview.off.step2')}</li>
      <li>{t('overview.off.step3')}</li>
    </ol>
    <div class="row">
      <Button variant="primary" icon="sliders" href={links.cacheSettings()}>{t('overview.off.enable')}</Button>
      <Button icon="drive" href={links.storage()}>{t('overview.off.storage')}</Button>
    </div>
  </div>
</div>

<style>
  .off {
    padding: var(--sp-2) var(--sp-4) var(--sp-2);
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    max-width: 70ch;
  }
  .lead {
    font-weight: 600;
  }
  ol {
    margin: 0;
    padding-left: var(--sp-5);
    color: var(--text-2);
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
  }
  .row {
    margin-top: var(--sp-2);
  }
</style>
