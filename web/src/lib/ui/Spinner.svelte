<!--
  @component
  Indeterminate progress indicator. <Spinner size={16} label="Loading" />
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'

  let { size = 20, label }: { size?: number; label?: string } = $props()
</script>

<span class="spinner" style:width="{size}px" style:height="{size}px" role="status" aria-label={label ?? t('common.state.loading')}></span>

<style>
  .spinner {
    display: inline-block;
    flex: none;
    border: 2px solid color-mix(in srgb, currentColor 25%, transparent);
    border-top-color: currentColor;
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .spinner {
      border-top-color: color-mix(in srgb, currentColor 25%, transparent);
      border-right-color: currentColor;
    }
  }
</style>
