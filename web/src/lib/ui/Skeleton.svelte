<!--
  @component
  Placeholder block while content loads. <Skeleton width="60%" height="14px" />
-->
<script lang="ts">
  let { width = '100%', height = '14px' }: { width?: string; height?: string } = $props()
</script>

<span class="skeleton" style:width style:height aria-hidden="true"></span>

<style>
  .skeleton {
    display: block;
    border-radius: 4px;
    background: linear-gradient(90deg, var(--surface-2) 25%, var(--surface-3) 50%, var(--surface-2) 75%);
    background-size: 200% 100%;
    animation: shimmer 1.4s ease-in-out infinite;
  }
  @keyframes shimmer {
    from {
      background-position: 100% 0;
    }
    to {
      background-position: -100% 0;
    }
  }
</style>
