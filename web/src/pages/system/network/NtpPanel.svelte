<!--
  @component
  The NTP server (settings.ntp): answering NTP clients with this machine's
  time and the stratum it reports while the clock is synchronised. Shows
  the ntp listener (PICACHE_NTP_LISTEN, off by default: without it nothing
  is answered; or why it could not be bound), the clock warning of the
  health check "ntp", and that answering every network in the DNS access
  settings makes it an open NTP server.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type Health, type ListenerRoleConfig, type NtpSettings, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Field, Notice, Panel, Skeleton, toast, Toggle } from '$lib/ui'
  import NumberInput from '../../dns/shared/NumberInput.svelte'
  import { rangeError, RANGES } from '../forms'
  import { nextAddresses, sameAddresses } from './listeners'

  interface Props {
    ntpRole?: ListenerRoleConfig
    /** GET /system/health (the check "ntp"). */
    health: Resource<Health>
    /** Why the ntp listener could not be bound at the last start (GET /system/info listeners.failed.ntp). */
    bindError?: string
  }

  let { ntpRole, health, bindError }: Props = $props()

  const form = settingsForm('ntp')
  // "Answer every network" (DNS settings › Access) opens the NTP server to the internet too.
  const openDns = resource((signal) => api.settings.get({ signal }).then((s) => s.dns.allowAllNetworks))

  const d = $derived(form.draft as NtpSettings | undefined)
  const clock = $derived(health.data?.checks.find((c) => c.name === 'ntp' && c.status !== 'ok'))
  const invalid = $derived(!!d && !!rangeError(d.stratum, RANGES.ntpStratum))
  const general = $derived(form.saveError && !form.saveError.field ? form.errorMessage : undefined)
  const bound = $derived(ntpRole?.bound ?? [])
  const next = $derived(ntpRole ? nextAddresses(ntpRole) : [])

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid) return
    if (await form.save()) {
      toast.success(t('common.state.saved'))
      void health.refresh()
    }
  }

  const formId = $props.id()
</script>

{#snippet footer()}
  <div class="row">
    <Button type="submit" form="ntp-{formId}" variant="primary" loading={form.saving} disabled={!form.dirty || invalid}>
      {t('common.action.save')}
    </Button>
    <Button variant="ghost" disabled={!form.dirty || form.saving} onclick={() => form.revert()}>{t('system.form.discard')}</Button>
  </div>
{/snippet}

<Panel id="net-ntp" title={t('system.network.ntp.title')} description={t('system.network.ntp.description')} footer={d && session.isAdmin ? footer : undefined}>
  {#if form.loadError && !d}
    <Notice tone="fail" title={t('system.network.ntp.loadError')}>{errorText(form.loadError)}</Notice>
  {:else if !d}
    <Skeleton height="220px" />
  {:else}
    <form id="ntp-{formId}" onsubmit={save} novalidate>
      <fieldset class="stack" disabled={!session.isAdmin}>
        {#if general}<Notice tone="fail">{general}</Notice>{/if}
        <Toggle bind:checked={d.enabled} id="net-field-ntp-enabled" label={t('system.network.ntp.enabled')} description={t('system.network.ntp.enabledHelp')} />
        <Field id="net-field-ntp-stratum" label={t('system.network.ntp.stratum')} help={t('system.network.ntp.stratumHelp', { def: form.defaults?.stratum ?? 3 })} error={form.error('stratum') ?? (invalid ? rangeError(d.stratum, RANGES.ntpStratum) : undefined)}>
          <NumberInput bind:value={d.stratum} min={RANGES.ntpStratum.min} max={RANGES.ntpStratum.max} />
        </Field>

        <div class="listener small">
          <span class="muted">{t('system.network.ntp.listener')}</span>
          {#if bound.length > 0}
            <span class="mono">{bound.join(', ')}</span>
          {:else if bindError}
            <span class="failed">{t('system.network.ntp.bindFailed', { error: bindError })}</span>
          {:else if next.length === 0}
            <span>{t('system.network.ntp.noListener')}</span>
          {:else}
            <span>{t('system.network.ntp.none')}</span>
          {/if}
          {#if ntpRole && !sameAddresses(next, bound)}
            <span class="muted">{next.length > 0 ? t('system.network.ntp.nextListener', { addresses: next.join(', ') }) : t('system.network.ntp.nextOff')}</span>
          {/if}
        </div>

        {#if d.enabled && bound.length === 0 && bindError}
          <Notice tone="fail" title={t('system.network.ntp.bindTitle')}>
            {t('system.network.ntp.bindText')}
            {#snippet actions()}
              <Button size="sm" variant="ghost" href={href('/system/network', { section: 'listeners' })}>{t('system.network.ntp.toListeners')}</Button>
            {/snippet}
          </Notice>
        {:else if d.enabled && bound.length === 0 && next.length === 0}
          <Notice tone="warn" title={t('system.network.ntp.offTitle')}>
            {t('system.network.ntp.offText')}
            {#snippet actions()}
              <Button size="sm" variant="ghost" href={href('/system/network', { section: 'listeners' })}>{t('system.network.ntp.toListeners')}</Button>
            {/snippet}
          </Notice>
        {/if}
        {#if d.enabled && clock?.message}
          <Notice tone="warn" title={t('system.network.ntp.clockTitle')}>{clock.message}</Notice>
        {/if}
        {#if d.enabled && openDns.data}
          <Notice tone="fail" title={t('system.network.ntp.openTitle')}>
            {t('system.network.ntp.openText')}
            {#snippet actions()}
              <Button size="sm" variant="ghost" href={href('/dns/settings', { section: 'access' })}>{t('system.network.ntp.toAccess')}</Button>
            {/snippet}
          </Notice>
        {/if}
      </fieldset>
    </form>
  {/if}
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .listener {
    display: flex;
    flex-wrap: wrap;
    gap: 2px var(--sp-2);
    overflow-wrap: anywhere;
  }
  .failed {
    color: var(--danger);
  }
</style>
