<!--
  @component
  Chip for a DNS query status in pair colours with a translated label.
  <QueryStatusChip status={q.status} />
-->
<script lang="ts">
  import { t, type MessageKey } from '../../i18n/index.svelte'
  import { queryStatusStyle } from '../traffic'
  import Chip from './Chip.svelte'

  let { status, size = 'sm' }: { status: string; size?: 'sm' | 'md' } = $props()

  const style = $derived(queryStatusStyle(status))
  const key = $derived(`common.queryStatus.${status}` as MessageKey)
  const label = $derived(t(key) === key ? status : t(key))
</script>

<Chip {size} pair={style.pair} striped={style.striped} tone={style.tone} {label} />
