<!--
  @component
  Other DHCP servers in the network: the search on demand (3 s) with its
  result (not in a container while DHCP is off: the ports open only at
  start there), the last search PiCache ran by itself, the servers seen
  recently and the explicit, warned "serve anyway" setting
  (ignoreOtherServers, saved with the page's save bar).
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { DhcpProbeResult, DhcpSettings, DhcpStatus } from '$lib/api'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Button, Checkbox, Notice, Panel, Spinner } from '$lib/ui'
  import OtherServerList from './OtherServerList.svelte'

  interface Props {
    form: SettingsForm<'dhcp'>
    status: DhcpStatus | undefined
    probing: boolean
    result?: DhcpProbeResult
    note?: { tone: 'info' | 'warn' | 'fail'; text: string }
    onprobe: () => void
    ondismissnote: () => void
  }

  let { form, status, probing, result, note, onprobe, ondismissnote }: Props = $props()

  const d = $derived(form.draft as DhcpSettings)
  const others = $derived(status?.otherServers ?? [])
  const last = $derived(status?.lastProbe)
  const available = $derived(!!status?.available)
  // A container opens the DHCP ports only at start: while DHCP is off the
  // search cannot open UDP 67 (PiCache searches by itself before serving).
  const containerOff = $derived(status?.deployment === 'docker' && status.state === 'off')
  const found = $derived(result?.servers ?? [])
</script>

<Panel id="dhcp-other" title={t('dns.dhcp.probe.title')} description={t('dns.dhcp.probe.description')}>
  {#snippet actions()}
    {#if session.isAdmin}
      <Button icon="search" loading={probing} disabled={!available || containerOff} onclick={onprobe}>{t('dns.dhcp.probe.button')}</Button>
    {/if}
  {/snippet}

  <div class="stack">
    {#if probing}
      <p class="running small" role="status"><Spinner size={14} />{t('dns.dhcp.probe.running')}</p>
    {:else if result}
      {#if found.length === 0}
        <Notice tone="ok">{t('dns.dhcp.probe.none')}</Notice>
      {:else}
        <Notice tone="warn" title={tn('dns.dhcp.probe.found', found.length)}>
          <ul class="found">
            {#each found as s (s.address)}
              <li>
                <span class="mono strong">{s.address}</span>
                {#if s.serverId && s.serverId !== s.address}<span>{t('dns.dhcp.other.serverId', { id: s.serverId })}</span>{/if}
                {#if s.offer}<span>{t('dns.dhcp.probe.offer', { address: s.offer })}</span>{/if}
                {#if s.mac}<span class="mono">{s.mac}</span>{/if}
              </li>
            {/each}
          </ul>
        </Notice>
      {/if}
    {/if}
    {#if note}
      <Notice tone={note.tone} ondismiss={ondismissnote}>{note.text}</Notice>
    {/if}

    {#if status && !status.available}
      <p class="small muted">{t('dns.dhcp.probe.notAvailable')}</p>
    {:else if last}
      <p class="small muted" title={formatDateTime(last.time)}>
        {last.servers === 0
          ? t('dns.dhcp.probe.last.none', { when: formatRelative(last.time) })
          : tn('dns.dhcp.probe.last', last.servers, { when: formatRelative(last.time) })}
      </p>
    {:else}
      <p class="small muted">{t('dns.dhcp.probe.never')}</p>
    {/if}
    {#if containerOff}
      <p class="small muted">{t('dns.dhcp.probe.containerOff')}</p>
    {/if}

    {#if others.length > 0}
      <div class="stack-sm">
        <h3 class="sub">{t('dns.dhcp.probe.seenTitle')}</h3>
        <OtherServerList servers={others} />
      </div>
    {/if}

    <fieldset class="plain stack-sm" disabled={!session.isAdmin}>
      <Checkbox bind:checked={d.ignoreOtherServers} label={t('dns.dhcp.ignore.label')} description={t('dns.dhcp.ignore.help')} />
      {#if d.ignoreOtherServers}
        <Notice tone="warn">{t('dns.dhcp.ignore.warn')}</Notice>
      {/if}
      {#if form.error('ignoreOtherServers')}<p class="err">{form.error('ignoreOtherServers')}</p>{/if}
    </fieldset>
  </div>
</Panel>

<style>
  .plain {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .running {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .found {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: var(--sp-1) 0 0;
    padding: 0;
    list-style: none;
  }
  .found li {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0 var(--sp-3);
  }
  .strong {
    color: var(--text);
    font-weight: 600;
  }
  h3.sub {
    font-size: var(--fs-sm);
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
