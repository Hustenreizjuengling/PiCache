<!--
  @component
  "Test a device": looks up a domain as a device would right now (POST
  /dns/lookup, not logged) and says whether it is blocked and why, with the
  parental controls, safe search and pause steps of the trace and a link
  to the full trace.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type Client, type GroupControls, type KnownClient, type LookupResult } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { href } from '$lib/router.svelte'
  import { isBlockedStatus } from '$lib/traffic'
  import { Button, Field, Input, Notice, Panel, QueryStatusChip, Select } from '$lib/ui'
  import { asciiDomain, isIP } from '../shared/input'

  interface Props {
    clients: readonly Client[] | undefined
    known: readonly KnownClient[] | undefined
    groups: readonly GroupControls[] | undefined
  }

  let { clients, known, groups }: Props = $props()

  const OTHER = 'other'

  /** Configured clients with an address to test as (an IP identifier or a recently seen address). */
  const devices = $derived(
    (clients ?? [])
      .map((c) => ({ id: c.id, name: c.name, ip: c.identifiers.find(isIP) ?? known?.find((k) => k.clientId === c.id)?.ip }))
      .sort((a, b) => a.name.localeCompare(b.name)),
  )

  let domain = $state('')
  let device = $state('')
  let address = $state('')
  let running = $state(false)
  let submitted = $state(false)
  let result = $state.raw<LookupResult | undefined>(undefined)
  let err = $state.raw<ApiError | undefined>(undefined)
  /** The device the result belongs to. */
  let testedAs = $state('')

  // Pick the first device with an address once the clients are known.
  const chosen = $derived(device || (devices.find((d) => d.ip) ? `c${devices.find((d) => d.ip)?.id}` : OTHER))
  const clientIp = $derived(chosen === OTHER ? address.trim() : (devices.find((d) => `c${d.id}` === chosen)?.ip ?? ''))

  const options = $derived([
    ...devices.map((d) => ({
      value: `c${d.id}`,
      label: d.ip ? `${d.name} (${d.ip})` : t('dns.parental.test.noAddress', { name: d.name }),
      disabled: !d.ip,
    })),
    { value: OTHER, label: t('dns.parental.test.otherAddress') },
  ])

  const domainError = $derived(
    fieldError(err, 'name') ?? (submitted && !domain.trim() ? t('common.field.required') : undefined),
  )
  const addressError = $derived(
    fieldError(err, 'clientIp') ??
      (submitted && chosen === OTHER && !isIP(address.trim()) ? t('dns.parental.test.addressRequired') : undefined),
  )
  const otherError = $derived(err && !err.field ? errorText(err) : undefined)

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = undefined
    const name = asciiDomain(domain)
    if (!name || !isIP(clientIp)) return
    domain = name
    running = true
    try {
      result = await api.dns.lookup({ name, type: 'A', clientIp })
      testedAs = clientIp
    } catch (x) {
      err = toApiError(x)
      result = undefined
    } finally {
      running = false
    }
  }

  // Dropped (no answer at all) counts as blocked here: the device gets nothing.
  const blocked = $derived(!!result && (isBlockedStatus(result.status) || result.status === 'dropped'))
  const verdict = $derived.by(() => {
    const r = result
    if (!r) return ''
    const reason = r.reason ?? ''
    switch (r.status) {
      case 'blocked-schedule':
      case 'blocked-service':
        return t('dns.parental.test.blockedParental', { reason })
      case 'dropped':
        // A blocked client (dns.blockedClients) or a dropped domain (dns.droppedDomains).
        return r.steps.some((s) => s.startsWith('blocked client: '))
          ? t('dns.parental.test.droppedClient', { reason })
          : t('dns.parental.test.droppedDomain', { name: r.name, reason })
      case 'blocked-upstream':
        return t('dns.parental.test.blockedUpstream', { reason })
      case 'blocked-rebind':
        return t('dns.parental.test.blockedRebind', { reason })
      case 'safesearch':
        return t('dns.parental.test.safeSearch', { reason })
      // E.g. safe search fails closed: the device gets SERVFAIL, never the unrestricted answer.
      case 'error':
        return reason ? t('dns.parental.test.error', { name: r.name, reason }) : t('dns.parental.test.errorNoReason', { name: r.name })
      case 'refused':
        return reason ? t('dns.parental.test.refused', { name: r.name, reason }) : t('dns.parental.test.refusedNoReason', { name: r.name })
      case 'blocked-list':
        // A protection list (category switch or list of a protection category) blocks at the parental step.
        if (r.steps.some((s) => s.startsWith('parental: blocked by list'))) return t('dns.parental.test.blockedProtection', { reason })
    }
    if (blocked) return r.reason ? t('dns.parental.test.blockedOther', { reason: r.reason }) : t('dns.parental.test.blockedOtherNoReason')
    return t('dns.parental.test.allowed', { name: r.name })
  })
  // The steps of parental controls, safe search and paused group filtering.
  const trace = $derived(result?.steps.filter((s) => /^(parental|safe search|filtering paused)\b/i.test(s)) ?? [])
  const groupNames = $derived(
    result ? result.groupIds.map((id) => groups?.find((g) => g.groupId === id)?.groupName ?? `#${id}`).join(', ') : '',
  )
</script>

<Panel title={t('dns.parental.test.title')} description={t('dns.parental.test.description')}>
  <div class="stack">
    <form class="form" onsubmit={submit} novalidate>
      <div class="domain">
        <Field label={t('common.label.domain')} required error={domainError}>
          <Input bind:value={domain} mono placeholder="youtube.com" maxlength={253} autocomplete="off" />
        </Field>
      </div>
      <div class="device">
        <Field label={t('dns.parental.test.device')}>
          <Select bind:value={() => chosen, (v) => (device = v)} {options} />
        </Field>
      </div>
      {#if chosen === OTHER}
        <div class="address">
          <Field label={t('dns.parental.test.address')} required error={addressError}>
            <Input bind:value={address} mono placeholder="192.168.1.20" maxlength={64} autocomplete="off" />
          </Field>
        </div>
      {/if}
      <div class="go">
        <Button type="submit" icon="search" loading={running}>{t('dns.parental.test.run')}</Button>
      </div>
    </form>

    {#if otherError}<Notice tone="fail">{otherError}</Notice>{/if}

    {#if result}
      <div class={['result', blocked && 'blocked']} aria-live="polite">
        <div class="row">
          <QueryStatusChip status={result.status} size="md" />
          <span class="mono small">{result.name}</span>
        </div>
        <p class="verdict">{verdict}</p>
        {#if groupNames}<p class="small muted">{t('dns.parental.test.groups', { groups: groupNames })}</p>{/if}
        {#each trace as step, i (i)}<p class="small muted mono">{step}</p>{/each}
        <a class="small" href={href('/dns/filtering', { tab: 'test', domain: result.name, client: testedAs })}>
          {t('dns.parental.test.fullTrace')}
        </a>
      </div>
    {/if}
  </div>
</Panel>

<style>
  .form {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .domain {
    flex: 2 1 220px;
  }
  .device {
    flex: 2 1 220px;
    min-width: 0;
  }
  .address {
    flex: 1 1 180px;
  }
  .go {
    padding-top: 26px;
  }
  .result {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line);
    border-left: 4px solid var(--blue);
    border-radius: var(--r-control);
  }
  .result.blocked {
    border-left-color: var(--orange);
  }
  .verdict {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  @media (max-width: 480px) {
    .domain,
    .device,
    .address {
      flex: 1 1 100%;
    }
    .go {
      padding-top: 0;
    }
  }
</style>
