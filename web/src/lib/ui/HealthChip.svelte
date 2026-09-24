<!--
  @component
  Chip for a health status (ok / warn / fail) with a translated label.
  <HealthChip status={check.status} />
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import type { HealthStatus } from '../api/types'
  import { healthTone } from '../traffic'
  import Chip from './Chip.svelte'

  let { status, label, size = 'sm' }: { status: HealthStatus; label?: string; size?: 'sm' | 'md' } = $props()
</script>

<Chip {size} tone={healthTone(status)} label={label ?? t(`common.health.${status}`)} />
