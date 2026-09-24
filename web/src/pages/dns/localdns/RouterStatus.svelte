<!--
  @component
  Router resolver status (/dns/router): the router answers the local domain,
  home.arpa and private reverse zones when no record or forwarder does.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { href } from '$lib/router.svelte'
  import { Button, Chip, KeyValue, Notice, Panel, Skeleton } from '$lib/ui'

  const router = resource((signal) => api.dns.router({ signal }), { interval: 60_000 })

  const modeLabel = $derived(
    router.data?.mode === 'auto'
      ? t('dns.router.mode.auto')
      : router.data?.mode === 'manual'
        ? t('dns.router.mode.manual')
        : t('dns.router.mode.off'),
  )
</script>

<Panel title={t('dns.router.title')} description={t('dns.router.description')}>
  {#snippet actions()}
    <Button size="sm" variant="ghost" icon="sliders" href={href('/dns/settings', { section: 'names' })}>{t('dns.router.change')}</Button>
  {/snippet}
  {#if router.data}
    {@const r = router.data}
    <div class="stack-sm">
      <div class="row">
        {#if r.mode === 'off'}
          <Chip tone="neutral" label={t('dns.router.off')} />
        {:else if r.answers}
          <Chip tone="ok" label={t('dns.router.answers')} />
        {:else}
          <Chip tone="warn" label={t('dns.router.notAnswering')} />
        {/if}
      </div>
      <KeyValue
        items={[
          { label: t('dns.router.mode'), value: modeLabel },
          { label: t('dns.router.address'), value: r.address, mono: true },
          { label: t('dns.router.domain'), value: r.domain, mono: true },
        ]}
      />
      {#if r.mode !== 'off' && !r.answers}
        <Notice tone="warn">{t('dns.router.notAnsweringText')}</Notice>
      {/if}
    </div>
  {:else if router.error}
    <Notice tone="fail">{errorText(router.error)}</Notice>
  {:else}
    <Skeleton height="72px" />
  {/if}
</Panel>
