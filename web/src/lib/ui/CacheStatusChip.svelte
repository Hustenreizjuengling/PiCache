<!--
  @component
  Chip for a cache request status (HIT green, MISS brown, …).
  <CacheStatusChip status={e.cacheStatus} />
-->
<script lang="ts">
  import { t, type MessageKey } from '../../i18n/index.svelte'
  import { cacheStatusStyle } from '../traffic'
  import Chip from './Chip.svelte'

  let { status, size = 'sm' }: { status: string; size?: 'sm' | 'md' } = $props()

  const style = $derived(cacheStatusStyle(status))
  const key = $derived(`common.cacheStatus.${status}` as MessageKey)
  const label = $derived(t(key) === key ? status : t(key))
</script>

<Chip {size} pair={style.pair} striped={style.striped} tone={style.tone} {label} />
