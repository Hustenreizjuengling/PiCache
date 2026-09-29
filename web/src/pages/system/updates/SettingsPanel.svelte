<!--
  @component
  Update settings (settings.updates): the daily check and the update
  channel (stable releases, also release candidates, or also the nightly
  builds of main). Choosing nightly asks to confirm its warning (whose way
  back names apt for a Debian package); the root helper installs nightlies
  only on hosts that allow them (install.sh --nightly), and Docker has
  none. Also names the proxy installs download
  through (the host's PICACHE_UPDATE_PROXY; not for a Debian package, which
  is downloaded by hand and installed with apt). Changes apply immediately.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { UpdateChannel, UpdateInfo } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { confirm, Field, Notice, Panel, Select, Skeleton, Toggle, toast } from '$lib/ui'

  let { info, onsaved }: { info?: UpdateInfo; onsaved?: () => void } = $props()

  const form = settingsForm('updates')

  const CHANNELS: UpdateChannel[] = ['stable', 'beta', 'nightly']
  const docker = $derived(info?.mode === 'docker')
  // A Debian package goes back with apt, not with picache update.
  const nightlyText = $derived(
    t(info?.mode === 'package' ? 'system.updates.settings.nightlyTextPackage' : 'system.updates.settings.nightlyText'),
  )

  const channelOptions = $derived(
    CHANNELS.map((c) => ({
      value: c,
      label: t(`system.updates.settings.channel.${c}`),
      // Nightly builds have no container image.
      disabled: c === 'nightly' && docker && form.saved?.channel !== 'nightly',
    })),
  )

  async function save(message: string): Promise<boolean> {
    if (await form.save()) {
      toast.success(message)
      onsaved?.()
      return true
    }
    toast.error(form.errorMessage ?? t('common.error.generic'))
    form.revert()
    return false
  }

  // The selector shows the choice while it is confirmed and saved, and
  // returns to the stored channel when that is cancelled or fails.
  let pick = $state<string>('stable')
  $effect(() => {
    const c = form.draft?.channel
    if (c) untrack(() => (pick = c))
  })

  async function setChannel(v: string): Promise<boolean> {
    const draft = form.draft
    if (!draft || !CHANNELS.includes(v as UpdateChannel)) return false
    if (v === draft.channel) return true
    const channel = v as UpdateChannel
    if (
      channel === 'nightly' &&
      !(await confirm({
        title: t('system.updates.settings.nightlyTitle'),
        message: nightlyText,
        confirmLabel: t('system.updates.settings.nightlyConfirm'),
      }))
    ) {
      return false
    }
    // Only the channel is sent: the server derives includePrereleases from it.
    draft.channel = channel
    return save(t('system.updates.settings.channelSaved', { channel: t(`system.updates.settings.channel.${channel}`) }))
  }

  async function choose(v: string) {
    pick = v
    if (!(await setChannel(v))) pick = form.draft?.channel ?? 'stable'
  }
</script>

<Panel id="updates-set-check" title={t('system.updates.settings.title')} description={t('system.updates.settings.description')}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.updates.settings.loadError')}>{form.loadError.message}</Notice>
  {:else if !form.draft}
    <Skeleton height="96px" />
  {:else}
    {@const channel = form.draft.channel}
    <div class="stack">
      <Toggle
        bind:checked={form.draft.checkEnabled}
        label={t('system.updates.settings.check')}
        description={t('system.updates.settings.checkHelp')}
        disabled={!session.isAdmin || form.saving}
        onchange={(on) => save(on ? t('system.updates.settings.checkOn') : t('system.updates.settings.checkOff'))}
      />
      <Field label={t('system.updates.settings.channel')} help={t(`system.updates.settings.channelHelp.${channel}`)}>
        <Select bind:value={() => pick, choose} options={channelOptions} disabled={!session.isAdmin || form.saving} />
      </Field>
      {#if channel === 'nightly'}
        <Notice tone="warn" title={t('system.updates.settings.nightlyTitle')}>{nightlyText}</Notice>
        {#if info && info.mode === 'helper' && !info.nightlyAllowed}
          <Notice tone="info" title={t('system.updates.settings.nightlyHostTitle')}>{t('system.updates.settings.nightlyHostText')}</Notice>
        {/if}
      {/if}
      {#if docker}
        <p class="small muted">{t('system.updates.settings.nightlyDocker')}</p>
      {:else if info && info.mode !== 'package'}
        <p class="small muted">
          {#if info.installProxy}
            {t('system.updates.settings.installProxy', { proxy: info.installProxy })}
          {:else}
            {t('system.updates.settings.installDirect')}
          {/if}
        </p>
      {/if}
    </div>
  {/if}
</Panel>
