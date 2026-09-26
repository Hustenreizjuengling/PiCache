<!--
  @component
  The scheduled backups stored in the current destination, newest first:
  download one (a plain link: the browser sends the session cookie) or
  delete it after a confirmation (only where the host allows destructive
  actions). Read-only principals see the list only.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, type ApiError, type ScheduledBackupFile } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatDateTimeShort } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, EmptyState, IconButton, Panel, Table, confirm, toast, type Column } from '$lib/ui'

  interface Props {
    files: readonly ScheduledBackupFile[] | undefined
    loading: boolean
    error: ApiError | undefined
    /** Why the destination could not be listed (e.g. the storage target is offline). */
    filesError?: string
    /** Directory of the current destination, if known. */
    path: string | undefined
    onretry: () => void
    onchanged: () => void
  }

  let { files, loading, error, filesError, path, onretry, onchanged }: Props = $props()

  function sentence(s: string): string {
    const x = s.trim()
    return x ? x[0].toUpperCase() + x.slice(1) : x
  }

  const tableError = $derived.by(() => {
    if (error && !files) return errorText(error)
    if (filesError) return `${t('system.backup.scheduled.files.unreadable')}: ${sentence(filesError)}`
    return undefined
  })

  async function remove(f: ScheduledBackupFile) {
    const ok = await confirm({
      title: t('system.backup.scheduled.files.deleteTitle', { name: f.name }),
      message: t('system.backup.scheduled.files.deleteText'),
      confirmLabel: t('system.backup.scheduled.files.delete'),
      action: () => api.backups.removeFile(f.name),
    })
    if (!ok) return
    toast.success(t('system.backup.scheduled.files.deleted'))
    onchanged()
  }

  const columns = $derived<Column<ScheduledBackupFile>[]>([
    // The whole name stays readable: it breaks on narrow screens instead of being cut off.
    { key: 'name', label: t('system.backup.scheduled.files.name'), mono: true, wrap: true, cell: nameCell, value: (f) => f.name },
    { key: 'time', label: t('system.backup.scheduled.files.written'), width: '1%', cell: timeCell, value: (f) => f.time },
    {
      key: 'size',
      label: t('common.label.size'),
      align: 'right',
      width: '1%',
      value: (f) => f.sizeBytes,
      format: (f) => formatBytes(f.sizeBytes),
    },
    ...(session.canOperate
      ? [{ key: 'actions', label: t('common.label.actions'), align: 'right', width: '1%', cell: actionsCell } satisfies Column<ScheduledBackupFile>]
      : []),
  ])

  const description = $derived(
    path
      ? `${t('system.backup.scheduled.files.in', { path })}. ${t('system.backup.scheduled.files.description')}`
      : t('system.backup.scheduled.files.description'),
  )
</script>

{#snippet nameCell(f: ScheduledBackupFile)}
  <span class="file">{f.name}</span>
{/snippet}

{#snippet timeCell(f: ScheduledBackupFile)}
  <span class="nowrap" title={formatDateTime(f.time, true)}>{formatDateTimeShort(f.time)}</span>
{/snippet}

{#snippet actionsCell(f: ScheduledBackupFile)}
  <span class="actions">
    <Button
      size="sm"
      variant="ghost"
      icon="download"
      href={api.backups.fileUrl(f.name)}
      download={f.name}
    >
      <!-- Links do not take aria-label here: name the file for screen readers. -->
      <span class="dl" aria-hidden="true">{t('system.backup.scheduled.files.download')}</span><span class="visually-hidden"
        >{t('system.backup.scheduled.files.downloadNamed', { name: f.name })}</span
      >
    </Button>
    {#if session.canDestroy}
      <IconButton
        icon="trash"
        size="sm"
        variant="danger"
        label={t('system.backup.scheduled.files.deleteNamed', { name: f.name })}
        onclick={() => remove(f)}
      />
    {/if}
  </span>
{/snippet}

<Panel title={t('system.backup.scheduled.files.title')} {description} flush>
  {#snippet actions()}
    {#if files && files.length > 0}
      <span class="small muted">{tn('system.backup.scheduled.files.count', files.length)}</span>
    {/if}
  {/snippet}
  <Table
    {columns}
    rows={files}
    key={(f) => f.name}
    loading={loading && !files}
    error={tableError}
    {onretry}
    caption={t('system.backup.scheduled.files.title')}
    skeletonRows={3}
  >
    {#snippet empty()}
      <EmptyState
        compact
        icon="archive"
        title={t('system.backup.scheduled.files.emptyTitle')}
        text={t('system.backup.scheduled.files.emptyText')}
      />
    {/snippet}
  </Table>
</Panel>

<style>
  .file {
    display: block;
    min-width: 20ch;
    padding: var(--sp-1) 0;
  }
  @media (max-width: 600px) {
    .dl {
      display: none;
    }
  }
  .actions {
    /* Contains the visually hidden link text, which would otherwise escape
       the table's scroll box and widen the page on phones. */
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
  }
</style>
