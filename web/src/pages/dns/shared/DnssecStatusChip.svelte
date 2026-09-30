<!--
  @component
  Chip for PiCache's DNSSEC verdict of an answer: secure (ok), insecure
  (neutral), bogus (fail), indeterminate (warn); `error` is a check of the
  DNSSEC test without a usable reply.
  <DnssecStatusChip status={event.dnssecStatus} />
-->
<script lang="ts">
  import { Chip } from '$lib/ui'
  import { dnssecStatusLabel, dnssecStatusTone, type DnssecChipStatus } from './dnssec'

  let { status, size = 'sm', title }: { status: DnssecChipStatus; size?: 'sm' | 'md'; title?: string } = $props()
</script>

<Chip {size} tone={dnssecStatusTone(status)} label={dnssecStatusLabel(status)} {title} />
