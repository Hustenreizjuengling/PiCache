<!--
  @component
  IPv6 DNS announcements of the DHCP server: router advertisements with
  RDNSS/DNSSL only and stateless DHCPv6 (settings dhcp.ipv6, saved with the
  page's save bar), each with its state, what blocks it and how to fix
  that (no ULA; router advertisements by reasonCode: a restart, CAP_NET_RAW
  for this kind of installation, an unverified capability drop, the
  reason), the announced address, the last advertisement and counters.
  Other routers and DHCPv6 servers that announce DNS, with a warning when
  one announces a DNS server that is not an address of this machine, and
  the last search. Notes: switch off the router's own
  announcement where possible; PiCache never becomes the default router and
  hands out no IPv6 addresses.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { DhcpSettings, DhcpStatus } from '$lib/api'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Chip, CopyButton, Icon, Notice, Panel, Toggle, Trans, type Tone } from '$lib/ui'
  import RestartButton from '../../system/RestartButton.svelte'
  import { REPO } from '../../system/health/about'
  import AnnouncerList from './AnnouncerList.svelte'

  interface Props {
    form: SettingsForm<'dhcp'>
    status: DhcpStatus | undefined
    /** PiCache answers again after a restart started here. */
    onrestarted?: () => void
  }

  let { form, status, onrestarted }: Props = $props()

  const installer = `curl -fsSL ${REPO}/releases/latest/download/get-picache.sh | sudo sh`

  const d = $derived(form.draft as DhcpSettings)
  const ra = $derived(status?.ipv6?.routerAdvertisements)
  const v6 = $derived(status?.ipv6?.dhcpv6)
  const serving = $derived(status?.state === 'serving')
  const ifaceName = $derived(status?.interface?.name ?? d.interface ?? '–')

  const TONE: Record<string, Tone> = { off: 'neutral', blocked: 'warn', sending: 'ok', serving: 'ok', error: 'fail' }

  function stateLabel(s: string): string {
    switch (s) {
      case 'off':
        return t('dns.dhcp.ipv6.state.off')
      case 'blocked':
        return t('dns.dhcp.ipv6.state.blocked')
      case 'sending':
        return t('dns.dhcp.ipv6.state.sending')
      case 'serving':
        return t('dns.dhcp.ipv6.state.serving')
    }
    return t('dns.dhcp.ipv6.state.error')
  }

  /** Server state is shown once the saved setting is on (or the server reports anything but off). */
  const showRa = $derived(!!ra && (ra.enabled || ra.state !== 'off'))
  const showV6 = $derived(!!v6 && (v6.enabled || v6.state !== 'off'))
  const raBlockers = $derived(ra?.blockers ?? [])
  const raUnavailable = $derived(!!ra && !ra.available)
  // dhcp-unavailable: the page's status notice explains; no-cap-net-raw is
  // explained even while the option is off (switching it on would not
  // help), and drop-unverified always (the health check fails with it).
  const raCode = $derived(raUnavailable ? (ra?.reasonCode ?? '') : '')
  const showRaProblem = $derived(
    raUnavailable &&
      raCode !== 'dhcp-unavailable' &&
      (showRa || d.ipv6.routerAdvertisements || raCode === 'no-cap-net-raw' || raCode === 'drop-unverified'),
  )
  // Older servers report the missing ULA of DHCPv6 only among the RA blockers.
  const v6Blockers = $derived(v6?.blockers ?? (v6?.state === 'blocked' && raBlockers.includes('no-ula') ? ['no-ula'] : []))
  /** The ULA hint is shown once: under router advertisements when they show it already. */
  const raShowsUla = $derived(!!ra && (showRa || d.ipv6.routerAdvertisements) && raBlockers.includes('no-ula'))
  const needsServer = $derived(!!status && !serving && (d.ipv6.routerAdvertisements || d.ipv6.dhcpv6))

  // ---- other announcers

  const announcers = $derived(status?.ipv6?.otherAnnouncers ?? [])
  const lastSearch = $derived(status?.ipv6?.lastSearch)
  const conflicts = $derived(announcers.filter((a) => a.conflict))
  // Every address of this machine is PiCache's own (ownDns), not only the
  // one its router advertisements announce.
  const conflictDns = $derived([
    ...new Set(conflicts.flatMap((a) => (a.dns ?? []).filter((x) => !(a.ownDns ?? []).includes(x)))),
  ])
  const showOthers = $derived(announcers.length > 0 || !!lastSearch)
</script>

{#snippet ulaPath()}<span class="path">{t('dns.network.fritz.ipv6Path')}</span>{/snippet}
{#snippet ulaField()}<strong>{t('dns.network.fritz.ula')}</strong>{/snippet}
{#snippet networkLink()}<a href={href('/dns/network')}>{t('dns.dhcp.ipv6.networkLink')}</a>{/snippet}

{#snippet blocker(b: string)}
  {#if b === 'no-ula'}
    {@render noUla()}
  {:else if b === 'no-interface'}
    <p class="small muted">{t('dns.dhcp.ipv6.noInterface')}</p>
  {:else if b === 'no-socket'}
    <p class="small muted">{t('dns.dhcp.ipv6.noSocket')}</p>
  {:else}
    <p class="small muted">{t('dns.dhcp.ipv6.blocker', { blocker: b })}</p>
  {/if}
{/snippet}

{#snippet noUla()}
  <div class="problem">
    <p>{t('dns.dhcp.ipv6.noUla', { interface: ifaceName })}</p>
    <p class="step">
      <Icon name="chevron-right" size={16} />
      <span><Trans key="dns.dhcp.ipv6.noUlaStep" path={ulaPath} field={ulaField} /></span>
    </p>
  </div>
{/snippet}

{#snippet raProblem(reason: string | undefined)}
  {#if raCode === 'restart-required'}
    <p>{t('dns.dhcp.ipv6.restart')}</p>
    {#if session.isAdmin}
      <div class="row">
        <RestartButton size="sm" message={t('dns.dhcp.ipv6.restartConfirm')} ondone={onrestarted} />
      </div>
    {/if}
  {:else if raCode === 'no-cap-net-raw' && status?.deployment === 'docker'}
    <p>{t('dns.dhcp.ipv6.noCap.docker')}</p>
  {:else if raCode === 'no-cap-net-raw' && status?.deployment === 'systemd'}
    <p>{t('dns.dhcp.ipv6.noCap.systemd')}</p>
    <div class="cmd">
      <code class="mono">{installer}</code>
      <CopyButton text={installer} />
    </div>
  {:else if raCode === 'no-cap-net-raw'}
    <p>{t('dns.dhcp.ipv6.noCap.other')}</p>
  {:else if raCode === 'drop-unverified'}
    <p>{t('dns.dhcp.ipv6.dropUnverified', { reason: reason ?? '–' })}</p>
  {:else}
    <p>{reason ? t('dns.dhcp.ipv6.unavailable', { reason }) : t('dns.dhcp.ipv6.unavailableNoReason')}</p>
  {/if}
{/snippet}

<Panel id="dhcp-ipv6" title={t('dns.dhcp.ipv6.title')} description={t('dns.dhcp.ipv6.description')}>
  <fieldset class="plain stack" disabled={!session.isAdmin}>
    <div class="feature stack-sm">
      <Toggle bind:checked={d.ipv6.routerAdvertisements} label={t('dns.dhcp.ipv6.ra')} description={t('dns.dhcp.ipv6.raHelp')} />
      {#if form.error('ipv6.routerAdvertisements')}<p class="sub err">{form.error('ipv6.routerAdvertisements')}</p>{/if}
      {#if ra && showRa}
        <div class="sub state">
          <Chip size="sm" tone={TONE[ra.state] ?? 'neutral'} label={stateLabel(ra.state)} />
          {#if ra.address}<span class="small">{t('dns.dhcp.ipv6.announces')} <span class="mono">{ra.address}</span></span>{/if}
          {#if ra.lastSent}
            <span class="small muted" title={formatDateTime(ra.lastSent)}>{t('dns.dhcp.ipv6.lastSent', { when: formatRelative(ra.lastSent) })}</span>
          {/if}
          {#if ra.sent > 0 || ra.solicitations > 0}
            <span class="small muted">
              {t('dns.dhcp.ipv6.counts', { sent: formatNumber(ra.sent), solicitations: formatNumber(ra.solicitations) })}
            </span>
          {/if}
        </div>
      {/if}
      {#if ra && showRaProblem}
        <div class="sub problem">{@render raProblem(ra.reason)}</div>
      {/if}
      {#if ra && (showRa || d.ipv6.routerAdvertisements)}
        <!-- A missing raw socket is explained above. -->
        {#each raBlockers.filter((b) => !(raUnavailable && b === 'no-raw-socket')) as b (b)}
          <div class="sub">
            {#if b === 'no-raw-socket'}
              <p class="small muted">{t('dns.dhcp.ipv6.noRawSocket')}</p>
            {:else}
              {@render blocker(b)}
            {/if}
          </div>
        {/each}
        {#if ra.state === 'error' && ra.error}<p class="sub err">{ra.error}</p>{/if}
      {/if}
    </div>

    <div class="feature stack-sm">
      <Toggle bind:checked={d.ipv6.dhcpv6} label={t('dns.dhcp.ipv6.dhcpv6')} description={t('dns.dhcp.ipv6.dhcpv6Help')} />
      {#if form.error('ipv6.dhcpv6')}<p class="sub err">{form.error('ipv6.dhcpv6')}</p>{/if}
      {#if v6 && showV6}
        <div class="sub state">
          <Chip size="sm" tone={TONE[v6.state] ?? 'neutral'} label={stateLabel(v6.state)} />
          {#if v6.replies > 0}<span class="small muted">{tn('dns.dhcp.ipv6.replies', v6.replies)}</span>{/if}
          {#if (v6.ignored ?? 0) > 0}<span class="small muted">{tn('dns.dhcp.ipv6.ignored', v6.ignored ?? 0)}</span>{/if}
        </div>
        {#each v6Blockers.filter((b) => !(raShowsUla && b === 'no-ula')) as b (b)}
          <div class="sub">{@render blocker(b)}</div>
        {/each}
        {#if v6.state === 'error' && v6.error}<p class="sub err">{v6.error}</p>{/if}
      {/if}
    </div>

    {#if needsServer}
      <p class="small muted">{t('dns.dhcp.ipv6.needsServer')}</p>
    {/if}

    {#if showOthers}
      <section class="others stack-sm" aria-labelledby="dhcp-ipv6-others">
        <h3 id="dhcp-ipv6-others">{t('dns.dhcp.ipv6.others.title')}</h3>
        {#if conflicts.length > 0}
          <Notice tone="warn" title={t('dns.dhcp.ipv6.others.conflictTitle')}>
            <Trans key="dns.dhcp.ipv6.others.conflictText" params={{ addresses: conflictDns.join(', ') || '–' }} link={networkLink} />
          </Notice>
        {/if}
        {#if announcers.length > 0}
          <AnnouncerList {announcers} />
        {:else}
          <p class="small muted">{t('dns.dhcp.ipv6.others.none')}</p>
        {/if}
        <p class="small muted">
          {#if lastSearch}
            <span title={formatDateTime(lastSearch.time)}>{t('dns.dhcp.ipv6.others.lastSearch', { when: formatRelative(lastSearch.time) })}</span>
          {/if}
          {#if lastSearch ? !lastSearch.ra : ra?.state !== 'sending'}{t('dns.dhcp.ipv6.others.raOnlyOwn')}{/if}
          {t('dns.dhcp.ipv6.others.help')}
        </p>
      </section>
    {/if}

    <ul class="notes small muted">
      <li>{t('dns.dhcp.ipv6.noteRouter')}</li>
      <li><Trans key="dns.dhcp.ipv6.noteFritz" link={networkLink} /></li>
    </ul>
  </fieldset>
</Panel>

<style>
  .plain {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  /* Aligned with the toggle's text (track 36 px + gap). */
  .sub {
    padding-left: calc(36px + var(--sp-3));
  }
  .state {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
    min-width: 0;
  }
  .state .mono {
    overflow-wrap: anywhere;
  }
  .problem {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    max-width: 90ch;
    font-size: var(--fs-sm);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .step {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    color: var(--text);
  }
  .step :global(.icon) {
    flex: none;
    margin-top: 1px;
    color: var(--text-3);
  }
  .path {
    font-style: italic;
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
    margin-top: var(--sp-1);
  }
  /* As the command on the Updates page. */
  .cmd {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
    margin-top: var(--sp-1);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .cmd code {
    flex: 1;
    min-width: 0;
    color: var(--text);
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
    user-select: all;
  }
  .others {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  .others h3 {
    font-size: var(--fs-sm);
  }
  .notes {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    max-width: 90ch;
    margin: 0;
    padding: var(--sp-3) 0 0 var(--sp-5);
    border-top: 1px solid var(--line);
  }
  @media (max-width: 480px) {
    .sub {
      padding-left: 0;
    }
  }
</style>
