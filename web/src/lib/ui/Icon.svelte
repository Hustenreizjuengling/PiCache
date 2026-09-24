<!--
  @component
  Inline SVG icon from lib/icons.ts. Decorative unless `label` is given.
  <Icon name="search" /> · <Icon name="alert" label="Warning" size={16} />
-->
<script lang="ts">
  import { icons, type IconName } from '../icons'

  let { name, size = 20, label }: { name: IconName; size?: number; label?: string } = $props()
</script>

<svg
  class="icon"
  width={size}
  height={size}
  viewBox="0 0 20 20"
  fill="none"
  stroke="currentColor"
  stroke-width="1.5"
  stroke-linecap="round"
  stroke-linejoin="round"
  role={label ? 'img' : undefined}
  aria-label={label}
  aria-hidden={label ? undefined : 'true'}
  focusable="false"><path d={icons[name]} /></svg>

<style>
  .icon {
    flex: none;
    display: inline-block;
    vertical-align: middle;
  }
</style>
