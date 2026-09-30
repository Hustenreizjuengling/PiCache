<!--
  @component
  Chip for whether an upstream returns DNSSEC data (its probe; validate
  mode): capable "DNSSEC" (ok), no-dnssec (warn), anchor-mismatch (fail),
  unknown (neutral). What the probe found wrong is its title.
  <UpstreamDnssecChip state={st.dnssec} error={st.dnssecError} />
-->
<script lang="ts">
  import type { UpstreamDnssecState } from '$lib/api'
  import { Chip } from '$lib/ui'
  import { upstreamDnssecLabel, upstreamDnssecTone } from './dnssec'

  let { state, error }: { state: UpstreamDnssecState; error?: string } = $props()
</script>

<Chip size="sm" tone={upstreamDnssecTone(state)} label={upstreamDnssecLabel(state)} title={error || undefined} />
