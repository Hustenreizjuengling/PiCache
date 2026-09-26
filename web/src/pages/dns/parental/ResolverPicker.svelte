<!--
  @component
  The family-safe resolver of a group on its parental controls card: off or
  one of the presets (admins choose, others read it). A group with its own
  upstreams lists them read-only with a link to Clients & groups; choosing
  a preset then asks before replacing them (the page does that).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { ClientGroup, UpstreamPreset } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Select } from '$lib/ui'
  import { presetName as nameOf } from '../shared/resolver'

  interface Props {
    group: ClientGroup
    presets: readonly UpstreamPreset[] | undefined
    busy?: boolean
    /** Sets the preset ("" = none); resolves true when saved. */
    onchange: (preset: string) => Promise<boolean>
  }

  let { group, presets, busy = false, onchange }: Props = $props()

  const auto = $props.id()
  const OWN = 'own'

  const own = $derived(group.upstreams)
  const current = $derived(group.upstreamPreset || (own.length > 0 ? OWN : ''))
  const presetName = $derived(nameOf(group.upstreamPreset, presets))

  /** The choice shown; back to the saved one when a change is cancelled or fails. */
  let pick = $state(untrack(() => current))
  $effect(() => {
    const c = current
    untrack(() => (pick = c))
  })

  const options = $derived([
    { value: '', label: t('dns.parental.resolver.off') },
    ...(presets ?? []).map((p) => ({ value: p.key, label: nameOf(p.key, presets) })),
    ...(own.length > 0 ? [{ value: OWN, label: t('dns.parental.resolver.own'), disabled: true }] : []),
  ])

  async function choose(v: string) {
    pick = v
    if (v === OWN || v === current) return
    if (!(await onchange(v))) pick = current
  }
</script>

<section class="stack-sm" aria-labelledby="pc-{auto}-resolver">
  <h3 id="pc-{auto}-resolver">{t('dns.parental.resolver.title')}</h3>
  {#if session.isAdmin}
    <div class="select">
      <Select
        size="sm"
        bind:value={() => pick, choose}
        {options}
        disabled={busy}
        aria-label={t('dns.parental.resolver.label', { group: group.name })}
      />
    </div>
  {:else}
    <p class="small">
      {group.upstreamPreset ? presetName : own.length > 0 ? t('dns.parental.resolver.own') : t('dns.parental.resolver.off')}
    </p>
  {/if}
  {#if own.length > 0}
    <p class="small muted">{t('dns.parental.resolver.ownText')}</p>
    <ul class="own mono small">
      {#each own as u (u)}<li>{u}</li>{/each}
    </ul>
    <a class="small" href={href('/dns/clients', { tab: 'groups', sel: group.id })}>{t('dns.parental.resolver.openGroup')}</a>
  {:else}
    <p class="small muted">{group.upstreamPreset ? t('dns.parental.resolver.onHelp') : t('dns.parental.resolver.offHelp')}</p>
  {/if}
</section>

<style>
  h3 {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .select {
    max-width: 320px;
  }
  .own {
    margin: 0;
    padding: 0;
    list-style: none;
    overflow-wrap: anywhere;
  }
</style>
