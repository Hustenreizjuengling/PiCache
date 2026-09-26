<!--
  @component
  Imports local records (admins) from a hosts file: every name of a line
  ("<address> <name> [<alias> …]") becomes an A or AAAA record for everyone
  or for the chosen groups. Loopback lines and the usual placeholder names
  (localhost, broadcasthost, …) are skipped; a record that exists already
  is unchanged; a pasted blocklist (0.0.0.0 lines) is refused.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, type ClientGroup, type RecordScope } from '$lib/api'
  import { formatNumber } from '$lib/format'
  import { Field, Select } from '$lib/ui'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import LineImportDialog from '../shared/LineImportDialog.svelte'

  interface Props {
    open?: boolean
    groups: readonly ClientGroup[] | undefined
    onimported: () => void
  }

  let { open = $bindable(false), groups, onimported }: Props = $props()

  const PLACEHOLDER = '192.168.1.10  nas.lan nas\n192.168.1.20  printer.lan   # 3c:22:fb:10:20:30\nfd00::10      nas.lan'

  let scope = $state<RecordScope>('all')
  let groupIds = $state<number[]>([])

  $effect.pre(() => {
    if (!open) return
    untrack(() => {
      scope = 'all'
      groupIds = []
    })
  })

  const scopeOptions = $derived([
    { value: 'all', label: t('dns.records.scope.all') },
    { value: 'groups', label: t('dns.records.scope.groups') },
  ])

  const fields: Record<string, () => string> = {
    syntax: () => t('dns.import.field.syntax'),
    name: () => t('common.label.name'),
    groupIds: () => t('common.label.groups'),
    scope: () => t('dns.records.scope'),
  }
</script>

<LineImportDialog
  bind:open
  title={t('dns.records.importTitle')}
  intro={t('dns.records.importIntro')}
  textLabel={t('dns.records.importText')}
  help={t('dns.records.importHelp')}
  placeholder={PLACEHOLDER}
  maxLines={10_000}
  accept=".txt,.hosts,text/plain"
  {fields}
  optionsKey={`${scope}:${groupIds.join(',')}`}
  blocked={scope === 'groups' && groupIds.length === 0}
  run={(text, dryRun) =>
    api.dns.records.import({ format: 'hosts', text, scope, groupIds: scope === 'groups' ? groupIds : [], dryRun })}
  doneText={(r) => t('dns.records.imported', { count: formatNumber(r.added) })}
  {onimported}
>
  {#snippet options()}
    <Field label={t('dns.records.scope')} help={scope === 'groups' ? t('dns.records.scopeHelp.groups') : t('dns.records.scopeHelp.all')}>
      <Select bind:value={() => scope, (v) => (scope = v === 'groups' ? 'groups' : 'all')} options={scopeOptions} />
    </Field>
    {#if scope === 'groups'}
      <GroupPicker {groups} bind:value={groupIds} emptyWarning={t('dns.records.importGroupsRequired')} />
    {/if}
  {/snippet}
</LineImportDialog>
