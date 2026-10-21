<!--
  @component
  Battle.net: no sign-in (Blizzard's downloads are public). The region whose
  servers and versions are used, the language of the installs, and the games
  the prefill downloads, with their current version.
-->
<script lang="ts">
  import { i18n, t } from '$i18n/index.svelte'
  import { api, resource, type PrefillSettings, type PrefillStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Field, Panel, Select, toast, type SelectOption } from '$lib/ui'
  import GameList, { type GameRow } from './GameList.svelte'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  const products = resource((signal) => api.prefill.battleNetProducts(false, { signal }), { interval: 60_000 })

  const rows: GameRow[] | undefined = $derived(
    products.data?.products.map((p) => ({
      id: p.code,
      name: p.name,
      sub: p.error ? `${p.code} · ${productError(p.error)}` : p.code,
      version: p.version,
      prefilled: p.prefilled,
      current: p.current,
    })),
  )

  /** The service's reasons in the UI language (others as they come). */
  function productError(e: string): string {
    if (e === 'not offered in this region') return t('cache.prefill.bnet.notOffered')
    if (e.startsWith('encrypted')) return t('cache.prefill.bnet.encrypted')
    return e
  }

  const REGIONS = ['eu', 'us', 'kr', 'tw'] as const
  const LOCALES = ['deDE', 'enGB', 'enUS', 'esES', 'esMX', 'frFR', 'itIT', 'jaJP', 'koKR', 'plPL', 'ptBR', 'ptPT', 'ruRU', 'thTH', 'trTR', 'zhCN', 'zhTW']
  const regionOptions: SelectOption[] = $derived(REGIONS.map((r) => ({ value: r, label: t(`cache.prefill.bnet.region.${r}`) })))
  const localeOptions: SelectOption[] = $derived(LOCALES.map((l) => ({ value: l, label: localeName(l) })))

  function localeName(l: string): string {
    const tag = `${l.slice(0, 2)}-${l.slice(2)}`
    try {
      return `${new Intl.DisplayNames([i18n.tag], { type: 'language' }).of(tag) ?? l} (${l})`
    } catch {
      return l
    }
  }

  async function setOption(key: 'region' | 'locale', value: string) {
    const s: PrefillSettings = structuredClone($state.snapshot(status.settings))
    if (key === 'region') s.battleNet.region = value as PrefillSettings['battleNet']['region']
    else s.battleNet.locale = value
    try {
      await api.prefill.saveSettings(s)
      toast.success(t('cache.prefill.settings.saved'))
      onchanged()
      void products.refresh()
    } catch (err) {
      toast.error(err)
    }
  }

  async function read() {
    const res = await api.prefill.battleNetProducts(true)
    products.set(res)
    if (res.error) toast.error(res.error)
  }

  async function save(ids: string[]) {
    await api.prefill.battleNetSelect(ids)
    onchanged()
    void products.refresh()
  }
</script>

<Panel title={t('cache.prefill.bnet.title')} description={t('cache.prefill.bnet.description')}>
  <div class="grid">
    <Field label={t('cache.prefill.bnet.regionLabel')} help={t('cache.prefill.bnet.regionHelp')}>
      <Select options={regionOptions} value={status.settings.battleNet.region} disabled={!session.isAdmin}
        onchange={(e) => setOption('region', (e.currentTarget as HTMLSelectElement).value)} />
    </Field>
    <Field label={t('cache.prefill.bnet.localeLabel')} help={t('cache.prefill.bnet.localeHelp')}>
      <Select options={localeOptions} value={status.settings.battleNet.locale} disabled={!session.isAdmin}
        onchange={(e) => setOption('locale', (e.currentTarget as HTMLSelectElement).value)} />
    </Field>
  </div>
</Panel>

<GameList
  store="Blizzard"
  title={t('cache.prefill.bnet.gamesTitle')}
  description={t('cache.prefill.bnet.gamesDescription')}
  {rows}
  saved={status.settings.battleNet.products}
  loading={products.loading}
  loadError={products.error ? errorText(products.error) : undefined}
  readError={products.data?.error || undefined}
  updated={products.data?.updated}
  onretry={() => products.refresh()}
  onread={read}
  onsave={save}
/>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: var(--sp-4);
  }
</style>
