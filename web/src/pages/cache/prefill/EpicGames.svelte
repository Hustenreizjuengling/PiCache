<!--
  @component
  The games of the Epic account (read from Epic at most every 12 hours, or
  now): which ones the prefill downloads, with their current version.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type PrefillStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { toast } from '$lib/ui'
  import GameList, { type GameRow } from './GameList.svelte'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  const apps = resource((signal) => api.prefill.epicApps(false, { signal }), { interval: 60_000 })

  const rows: GameRow[] | undefined = $derived(
    apps.data?.apps.map((a) => ({ id: a.id, name: a.name, version: a.version, prefilled: a.prefilled, current: a.current })),
  )

  async function read() {
    const res = await api.prefill.epicApps(true)
    apps.set(res)
    if (res.error) toast.error(res.error)
  }

  async function save(ids: string[]) {
    await api.prefill.epicSelect(ids)
    onchanged()
    void apps.refresh()
  }
</script>

<GameList
  store="Epic"
  title={t('cache.prefill.epic.gamesTitle')}
  description={t('cache.prefill.epic.gamesDescription')}
  {rows}
  saved={status.settings.epic.apps}
  loading={apps.loading}
  loadError={apps.error ? errorText(apps.error) : undefined}
  readError={apps.data?.error || undefined}
  updated={apps.data?.updated}
  onretry={() => apps.refresh()}
  onread={read}
  onsave={save}
/>
