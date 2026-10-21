<!--
  @component
  The games of the Steam account (read from Steam at most every 12 hours,
  or now with "Read from Steam"): which ones the prefill downloads, with
  their download size for the chosen platforms and languages.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type PrefillStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { toast } from '$lib/ui'
  import GameList, { type GameRow } from './GameList.svelte'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  const apps = resource((signal) => api.prefill.steamApps(false, { signal }), { interval: 60_000 })

  const rows: GameRow[] | undefined = $derived(
    apps.data?.apps.map((a) => ({ id: String(a.id), name: a.name, sub: String(a.id), bytes: a.bytes, prefilled: a.prefilled, current: a.current })),
  )
  const saved = $derived(status.settings.steam.apps.map(String))

  async function read() {
    const res = await api.prefill.steamApps(true)
    apps.set(res)
    if (res.error) toast.error(res.error)
  }

  async function save(ids: string[]) {
    await api.prefill.steamSelect(ids.map(Number))
    onchanged()
    void apps.refresh()
  }
</script>

<GameList
  store="Steam"
  title={t('cache.prefill.games.title')}
  description={t('cache.prefill.games.description')}
  {rows}
  {saved}
  sizes
  loading={apps.loading}
  loadError={apps.error ? errorText(apps.error) : undefined}
  readError={apps.data?.error || undefined}
  updated={apps.data?.updated}
  onretry={() => apps.refresh()}
  onread={read}
  onsave={save}
/>
