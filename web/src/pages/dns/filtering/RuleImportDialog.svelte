<!--
  @component
  Imports your rules (admins) from list lines: domain rules, hosts lines,
  regular expressions and their options ($dnstype, $denyallow, $reply,
  $dnsrewrite with an address, $invert, the ;querytype and ;reply
  suffixes). The imported rules apply to the chosen groups (Default
  preselected), are enabled and have no comment. A line whose rule exists
  with the same options is unchanged.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, type ClientGroup } from '$lib/api'
  import { formatNumber } from '$lib/format'
  import GroupPicker from '../shared/GroupPicker.svelte'
  import LineImportDialog from '../shared/LineImportDialog.svelte'

  interface Props {
    open?: boolean
    groups: readonly ClientGroup[] | undefined
    onimported: () => void
  }

  let { open = $bindable(false), groups, onimported }: Props = $props()

  const PLACEHOLDER = '||ads.example.com^\n@@||cdn.example.com^\n||tracker.example^$dnstype=A|AAAA\n/^ad[sx]?[0-9]*\\./\n0.0.0.0 telemetry.example.net'

  let groupIds = $state<number[]>([DEFAULT_GROUP_ID])

  $effect.pre(() => {
    if (!open) return
    untrack(() => (groupIds = [DEFAULT_GROUP_ID]))
  })

  const fields: Record<string, () => string> = {
    syntax: () => t('dns.import.field.syntax'),
    pattern: () => t('dns.rules.pattern'),
    qtypes: () => t('dns.rules.qtypes'),
    qtypesNegate: () => t('dns.rules.qtypes'),
    reply: () => t('dns.rules.reply'),
    replyIpv4: () => t('dns.rules.replyIpv4'),
    replyIpv6: () => t('dns.rules.replyIpv6'),
    denyallow: () => t('dns.rules.denyallow'),
    invert: () => t('dns.rules.badge.invert'),
    groupIds: () => t('common.label.groups'),
  }
</script>

<LineImportDialog
  bind:open
  title={t('dns.rules.importTitle')}
  intro={t('dns.rules.importIntro')}
  textLabel={t('dns.rules.importText')}
  help={t('dns.rules.importHelp')}
  placeholder={PLACEHOLDER}
  maxLines={20_000}
  accept=".txt,.list,text/plain"
  {fields}
  optionsKey={groupIds.join(',')}
  run={(text, dryRun) => api.filter.rules.import({ text, groupIds, dryRun })}
  doneText={(r) => t('dns.rules.imported', { count: formatNumber(r.added) })}
  {onimported}
>
  {#snippet options()}
    <GroupPicker {groups} bind:value={groupIds} help={t('dns.rules.importGroupsHelp')} />
  {/snippet}
</LineImportDialog>
