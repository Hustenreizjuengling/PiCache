<!--
  @component
  PiCache wordmark: four conductor pairs and the name.
-->
<script lang="ts">
  let { size = 'md' }: { size?: 'md' | 'lg' } = $props()
</script>

<span class={['brand', size]}>
  <svg class="mark" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
    <rect x="2" y="5" width="4" height="14" rx="1.2" fill="var(--blue)" />
    <rect x="7.5" y="5" width="4" height="14" rx="1.2" fill="var(--orange)" />
    <rect x="13" y="5" width="4" height="14" rx="1.2" fill="var(--green)" />
    <rect x="18.5" y="5" width="4" height="14" rx="1.2" fill="var(--brown)" />
  </svg>
  <span class="name">PiCache</span>
</span>

<style>
  .brand {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    color: var(--text);
  }
  .mark {
    width: 24px;
    height: 24px;
  }
  .name {
    font-size: var(--fs-lg);
    font-weight: 700;
    letter-spacing: 0;
  }
  .lg .mark {
    width: 32px;
    height: 32px;
  }
  .lg .name {
    font-size: var(--fs-xl);
  }
</style>
