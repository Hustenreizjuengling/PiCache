<!--
  @component
  The interfaces of this machine (GET /network/interfaces, loopback left
  out), one card each: name with its state and whether it is virtual, MAC
  address and link (speed and duplex only when the driver reports them,
  MTU), the addresses, the networks routed through it (the source networks
  an iface:<name> client identifier covers), default gateways and the
  traffic counters. In a container bridge network these are the
  container's own interfaces; on systems other than Linux the list is
  empty.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type NetworkInterface } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatNumber } from '$lib/format'
  import { Badge, Button, Chip, EmptyState, Notice, Panel, Skeleton, type Tone } from '$lib/ui'

  const data = resource((signal) => api.network.interfaces({ signal }), { interval: 60_000 })

  const list = $derived(data.data?.interfaces)

  /** State chip: administratively down, up (operstate "unknown" is normal for tunnels such as wg0), or the operstate. */
  function state(i: NetworkInterface): { tone: Tone; label: string } {
    if (!i.up) return { tone: 'neutral', label: t('dns.network.ifaces.state.disabled') }
    if (i.operState === 'up' || i.operState === 'unknown') return { tone: 'ok', label: t('dns.network.ifaces.state.up') }
    return { tone: 'warn', label: t(`dns.network.ifaces.state.${i.operState}`) }
  }

  function speed(mbps: number): string {
    return mbps >= 1000 && mbps % 1000 === 0
      ? t('dns.network.ifaces.gbits', { n: formatNumber(mbps / 1000) })
      : t('dns.network.ifaces.mbits', { n: formatNumber(mbps) })
  }

  function link(i: NetworkInterface): string {
    const parts: string[] = []
    if (i.speedMbps) parts.push(speed(i.speedMbps))
    if (i.duplex) parts.push(t(`dns.network.ifaces.${i.duplex}`))
    parts.push(t('dns.network.ifaces.mtu', { mtu: i.mtu }))
    return parts.join(' · ')
  }
</script>

{#snippet addresses(items: string[])}
  {#if items.length > 0}
    <span class="list mono">
      <!-- Each once: a key may appear only once (two default routes via one gateway). -->
      {#each [...new Set(items)] as a (a)}<span>{a}</span>{/each}
    </span>
  {:else}
    <span class="subtle">–</span>
  {/if}
{/snippet}

<Panel id="network-interfaces" title={t('dns.network.ifaces.title')} description={t('dns.network.ifaces.description')}>
  {#if data.error && !data.data}
    <Notice tone="warn">
      {t('dns.network.ifaces.unavailable', { reason: errorText(data.error) })}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => data.refresh()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !list}
    <Skeleton height="160px" />
  {:else}
    <div class="stack">
      {#if data.data?.mode === 'bridge'}
        <Notice tone="info">{t('dns.network.ifaces.bridge')}</Notice>
      {/if}
      {#if list.length === 0}
        <EmptyState compact icon="network" title={t('dns.network.ifaces.empty')} text={t('dns.network.ifaces.emptyText')} />
      {:else}
        <ul class="cards">
          {#each list as i (i.name)}
            {@const st = state(i)}
            <li class="card" aria-labelledby="iface-{i.index}-{i.name}">
              <div class="head">
                <h3 id="iface-{i.index}-{i.name}" class="mono">{i.name}</h3>
                <Chip size="sm" tone={st.tone} label={st.label} />
                {#if i.virtual}<Badge title={t('dns.network.ifaces.virtualHelp')}>{t('dns.network.ifaces.virtual')}</Badge>{/if}
              </div>
              <p class="small muted link">
                {#if i.mac}<span class="mono">{i.mac}</span> · {/if}{link(i)}
              </p>
              <dl>
                <dt>{t('dns.network.ifaces.addresses')}</dt>
                <dd>{@render addresses(i.addresses)}</dd>
                <dt title={t('dns.network.ifaces.networksHelp')}>{t('dns.network.ifaces.networks')}</dt>
                <dd>{@render addresses(i.networks)}</dd>
                <dt>{t('dns.network.ifaces.gateway')}</dt>
                <dd>{@render addresses(i.defaultGateways.map((g) => g.gateway))}</dd>
                <dt>{t('dns.network.ifaces.traffic')}</dt>
                <dd class="traffic">
                  <span>{t('dns.network.ifaces.received', { bytes: formatBytes(i.rxBytes) })}</span>
                  <span>{t('dns.network.ifaces.sent', { bytes: formatBytes(i.txBytes) })}</span>
                  {#if i.rxErrors + i.txErrors > 0}
                    <span class="errors">{tn('dns.network.ifaces.errors', i.rxErrors + i.txErrors)}</span>
                  {/if}
                </dd>
              </dl>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</Panel>

<style>
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 340px), 1fr));
    gap: var(--sp-3);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .card {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .link {
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--sp-1) var(--sp-3);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    color: var(--text-2);
  }
  dd {
    min-width: 0;
    margin: 0;
  }
  .list {
    display: flex;
    flex-direction: column;
    line-height: 1.35;
    overflow-wrap: anywhere;
  }
  .traffic {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-3);
  }
  .errors {
    color: var(--warning);
  }
  @media (max-width: 480px) {
    dl {
      grid-template-columns: minmax(0, 1fr);
      gap: 0;
    }
    dd {
      margin-bottom: var(--sp-2);
    }
  }
</style>
