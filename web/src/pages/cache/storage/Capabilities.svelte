<!--
  @component
  What this environment allows for storage: container type, systemd, the
  root helper for mounting shares, kernel file systems and mount helpers,
  and the user IDs files are written as (for chown on the host or NAS).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, StorageCapabilities } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { Chip, KeyValue, Notice, Panel, Skeleton } from '$lib/ui'

  let { caps, error }: { caps: StorageCapabilities | undefined; error: ApiError | undefined } = $props()

  function names(m: Record<string, boolean> | undefined): string[] {
    return Object.entries(m ?? {})
      .filter(([, on]) => on)
      .map(([k]) => k)
      .sort()
  }

  const environment = $derived.by(() => {
    const c = caps?.container ?? ''
    switch (c) {
      case 'docker':
        return t('cache.caps.docker')
      case 'podman':
        return t('cache.caps.podman')
      case 'lxc':
        return t('cache.caps.lxc')
      case '':
        return t('cache.caps.bareMetal')
      default:
        return c
    }
  })

  const mapped = $derived(!!caps && !caps.initUserNs && (caps.uidMapOffset > 0 || caps.gidMapOffset > 0))
</script>

<Panel title={t('cache.caps.title')} description={t('cache.caps.description')}>
  {#if error && !caps}
    <Notice tone="fail">{errorText(error)}</Notice>
  {:else if !caps}
    <Skeleton height="120px" />
  {:else}
    <div class="stack">
      <KeyValue
        items={[
          { label: t('cache.caps.environment'), value: environment },
          { label: t('cache.caps.os'), value: caps.os },
          { label: t('cache.caps.dockerNetwork'), value: caps.dockerMode },
          { label: t('cache.caps.systemd'), value: caps.systemd ? t('common.state.yes') : t('common.state.no') },
          { label: t('cache.caps.mountRoot'), value: caps.mountRoot, mono: true },
          { label: t('cache.caps.runsAs'), value: `${caps.uid}:${caps.gid}`, mono: true },
          {
            label: t('cache.caps.hostIds'),
            value: mapped ? `${caps.uidMapOffset + caps.uid}:${caps.gidMapOffset + caps.gid}` : undefined,
            mono: true,
          },
        ]}
      >
        <dt>{t('cache.caps.hostApply')}</dt>
        <dd>
          {#if caps.hostApply}
            <Chip size="sm" tone="ok" label={t('cache.caps.hostApplyYes')} />
          {:else}
            <Chip size="sm" tone="neutral" label={t('cache.caps.hostApplyNo')} />
          {/if}
        </dd>
        <dt>{t('cache.caps.filesystems')}</dt>
        <dd class="mono">{names(caps.filesystems).join(', ') || '–'}</dd>
        <dt>{t('cache.caps.helpers')}</dt>
        <dd class="mono">{names(caps.mountHelpers).join(', ') || '–'}</dd>
      </KeyValue>
      {#if mapped}
        <p class="muted small">{t('cache.caps.hostIdsHelp', {
            uid: String(caps.uidMapOffset + caps.uid),
            gid: String(caps.gidMapOffset + caps.gid),
          })}</p>
      {/if}
      {#if caps.dockerMode === 'bridge'}
        <Notice tone="warn">{t('cache.caps.bridgeWarning')}</Notice>
      {/if}
    </div>
  {/if}
</Panel>
