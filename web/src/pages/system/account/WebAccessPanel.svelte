<!--
  @component
  Web access (settings.web): whether only this machine, private and
  connected networks and the listed networks may use the web UI, the
  trusted reverse proxies whose X-Forwarded-For is read, and the oldest
  TLS version of the HTTPS port. Shows the address PiCache sees for this
  browser (with a warning when that is this machine although the page was
  opened by another address: an untrusted reverse proxy on the host), how
  many connections it refused, and the host command that undoes a
  lock-out. The server refuses a change that would lock out this
  browser; its message appears at the field. The web interface panel below
  edits the same section with a form of its own; each sends only the
  members changed in it.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { formatNumber } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, CopyButton, Field, Notice, Panel, Select, Skeleton, Toggle, Trans, toast } from '$lib/ui'
  import { lineError } from '../../dns/shared/errors'
  import LinesInput from '../../dns/shared/LinesInput.svelte'

  /** Most entries the server accepts. */
  const MAX_NETWORKS = 64
  const MAX_PROXIES = 16

  // Always allowed while the restriction is on, besides this machine (internal/netutil).
  const PRIVATE_RANGES = ['10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16', '100.64.0.0/10', 'fc00::/7']
  const RESET = 'sudo picache web-access --reset'
  const RESET_DOCKER = 'docker exec -u 65532:65532 <container> /picache web-access --reset'

  const form = settingsForm('web')
  const info = resource((signal) => api.system.info({ signal }), { interval: 60_000 })

  const d = $derived(form.draft)
  const local = $derived({
    networks:
      d && d.allowedNetworks.length > MAX_NETWORKS ? t('system.webAccess.tooMany', { max: MAX_NETWORKS }) : undefined,
    proxies: d && d.trustedProxies.length > MAX_PROXIES ? t('system.webAccess.tooMany', { max: MAX_PROXIES }) : undefined,
  })
  const invalid = $derived(!!(local.networks || local.proxies))

  const MEMBERS = ['restrictToNetworks', 'allowedNetworks', 'trustedProxies', 'tlsMinVersion'] as const

  const errors = $derived({
    restrict: form.error('restrictToNetworks'),
    networks: lineError(form.saveError, 'web.allowedNetworks') ?? local.networks,
    proxies: lineError(form.saveError, 'web.trustedProxies') ?? local.proxies,
    tls: form.error('tlsMinVersion'),
  })
  const general = $derived(
    form.saveError && !MEMBERS.some((m) => form.saveError?.field?.startsWith(`web.${m}`)) ? form.errorMessage : undefined,
  )

  const tlsOptions = $derived([
    { value: '1.2', label: t('system.webAccess.tls12') },
    { value: '1.3', label: t('system.webAccess.tls13') },
  ])

  const clientAddr = $derived(info.data?.clientAddress)
  const peerAddr = $derived(info.data?.peerAddress)

  const isLoopback = (a: string) => a === '::1' || a.startsWith('127.')
  // PiCache sees this browser as loopback although the page was not opened
  // on this machine: a reverse proxy on the host that is not trusted makes
  // every client loopback (always allowed, one sign-in throttle for all).
  const viaUntrustedLocalProxy = $derived(
    !!clientAddr && isLoopback(clientAddr) && !['localhost', '127.0.0.1', '[::1]'].includes(location.hostname),
  )

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid) return
    if (!(await form.save())) return
    toast.success(t('common.state.saved'))
    void info.refresh()
  }

  const formId = $props.id()
</script>

{#snippet saveBar()}
  <div class="row">
    <Button type="submit" form="web-access-{formId}" variant="primary" loading={form.saving} disabled={!form.dirty || invalid}>
      {t('common.action.save')}
    </Button>
    <Button variant="ghost" disabled={!form.dirty || form.saving} onclick={() => form.revert()}>
      {t('system.form.discard')}
    </Button>
  </div>
{/snippet}

<!-- Read-only principals (and admins while the host locks the configuration) get no save bar. -->
<Panel
  id="web-access"
  title={t('system.webAccess.title')}
  description={t('system.webAccess.description')}
  footer={d && session.isAdmin ? saveBar : undefined}
>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.web.loadError')}>{form.loadError.message}</Notice>
  {:else if !d}
    <Skeleton height="260px" />
  {:else}
    <form id="web-access-{formId}" class="stack" onsubmit={save} novalidate>
      {#if general}<Notice tone="fail">{general}</Notice>{/if}

      {#if info.data}
        <div class="seen small">
          {#if clientAddr}
            <p>
              <Trans key={peerAddr && peerAddr !== clientAddr ? 'system.webAccess.seenViaProxy' : 'system.webAccess.seen'}>
                {#snippet client()}<code class="mono">{clientAddr}</code>{/snippet}
                {#snippet peer()}<code class="mono">{peerAddr}</code>{/snippet}
              </Trans>
            </p>
          {/if}
          <p class="muted">{tn('system.webAccess.refused', info.data.webRefused, { count: formatNumber(info.data.webRefused) })}</p>
        </div>
      {/if}
      {#if viaUntrustedLocalProxy && session.canOperate}
        <Notice tone="warn" title={t('system.webAccess.loopbackTitle')}>{t('system.webAccess.loopbackText')}</Notice>
      {/if}

      <div class="stack-sm">
        <Toggle
          bind:checked={d.restrictToNetworks}
          label={t('system.webAccess.restrict')}
          description={t('system.webAccess.restrictHelp')}
          disabled={!session.isAdmin}
        />
        {#if errors.restrict}<p class="small err">{errors.restrict}</p>{/if}
      </div>
      {#if !d.restrictToNetworks && session.canOperate}
        <Notice tone="info" title={t('system.webAccess.recommendTitle')}>{t('system.webAccess.recommendText')}</Notice>
      {/if}

      <div class="always small">
        <p class="muted">{t('system.webAccess.alwaysTitle')}</p>
        <ul>
          <li>{t('system.webAccess.always.host')}</li>
          <li>
            {t('system.webAccess.always.private')}
            {#each PRIVATE_RANGES as r, i (r)}<code class="mono">{r}</code>{i < PRIVATE_RANGES.length - 1 ? ', ' : ''}{/each}
            {t('system.webAccess.always.linkLocal')}
          </li>
          <li>{t('system.webAccess.always.connected')}</li>
          <li>{t('system.webAccess.always.dns')}</li>
          <li>{t('system.webAccess.always.proxies')}</li>
        </ul>
      </div>

      <div class="grid">
        <Field label={t('system.webAccess.networks')} optional error={errors.networks} help={t('system.webAccess.networksHelp')}>
          <LinesInput bind:value={d.allowedNetworks} rows={4} placeholder={'203.0.113.0/24\n2001:db8:42::/48'} disabled={!session.isAdmin} />
        </Field>
        <Field label={t('system.webAccess.proxies')} optional error={errors.proxies} help={t('system.webAccess.proxiesHelp')}>
          <LinesInput bind:value={d.trustedProxies} rows={4} placeholder="192.168.1.2" disabled={!session.isAdmin} />
        </Field>
      </div>
      {#if d.trustedProxies.length > 0}
        <Notice tone="warn" title={t('system.webAccess.proxyWarnTitle')}>
          <ul>
            <li>{t('system.webAccess.proxyWarn.exact')}</li>
            <li>{t('system.webAccess.proxyWarn.append')}</li>
            <li>{t('system.webAccess.proxyWarn.loopback')}</li>
          </ul>
        </Notice>
      {/if}

      <Field
        label={t('system.webAccess.tlsMin')}
        error={errors.tls}
        help={d.tlsMinVersion === '1.3' ? t('system.webAccess.tls13Help') : t('system.webAccess.tlsHelp')}
      >
        <Select
          options={tlsOptions}
          bind:value={() => d.tlsMinVersion, (v) => (d.tlsMinVersion = v === '1.3' ? '1.3' : '1.2')}
          disabled={!session.isAdmin}
        />
      </Field>

      <details class="reset">
        <summary class="small">{t('system.webAccess.resetTitle')}</summary>
        <div class="stack-sm">
          <p class="small muted">{t('system.webAccess.resetText')}</p>
          <div class="cmd">
            <code class="mono">{RESET}</code>
            <CopyButton text={RESET} />
          </div>
          <p class="small muted">{t('system.webAccess.resetDocker')}</p>
          <div class="cmd">
            <code class="mono">{RESET_DOCKER}</code>
            <CopyButton text={RESET_DOCKER} />
          </div>
        </div>
      </details>
    </form>
  {/if}
</Panel>

<style>
  .seen {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--sp-2) var(--sp-3);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .seen code {
    color: var(--text);
    overflow-wrap: anywhere;
  }
  .always ul {
    margin: var(--sp-1) 0 0;
    padding-left: var(--sp-5);
    color: var(--text-2);
  }
  .always li + li {
    margin-top: 2px;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  @media (max-width: 700px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .err {
    color: var(--danger);
  }
  .reset summary {
    cursor: pointer;
    color: var(--text-2);
    width: fit-content;
  }
  .reset[open] summary {
    margin-bottom: var(--sp-2);
  }
  .cmd {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .cmd code {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-sm);
  }
</style>
