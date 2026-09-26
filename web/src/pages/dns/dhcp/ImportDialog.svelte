<!--
  @component
  Imports reserved addresses (admins): a format (CSV as exported, hosts
  file, "MAC IP [name]" lines), the list pasted or loaded from a file, and
  "replace all" (deletes reservations whose MAC address is not in the list).
  Preview checks the list on the server (dryRun: what it would add, change
  and delete, and every refused line); Apply is enabled only after a preview
  of exactly this input without errors, and the server checks it again: if
  it still refuses, its errors are shown and nothing is saved.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type DhcpImportError, type DhcpImportFormat, type DhcpImportResult } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatNumber } from '$lib/format'
  import { Button, Checkbox, Dialog, Field, Notice, Select, Table, Textarea, toast, type Column, type SelectOption } from '$lib/ui'

  interface Props {
    open?: boolean
    onimported: () => void
  }

  let { open = $bindable(false), onimported }: Props = $props()

  /** The server's limit for the text (256 KiB). */
  const MAX_BYTES = 256 * 1024

  let format = $state<DhcpImportFormat>('csv')
  let text = $state('')
  let replace = $state(false)
  let busy = $state<'preview' | 'apply' | undefined>(undefined)
  let result = $state.raw<DhcpImportResult | undefined>(undefined)
  /** The input the result belongs to. */
  let resultFor = $state('')
  /** The result is the server refusing an Apply. */
  let refused = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let fileError = $state<string | undefined>(undefined)
  let fileInput: HTMLInputElement | undefined = $state()

  $effect.pre(() => {
    if (!open) return
    untrack(() => {
      format = 'csv'
      text = ''
      replace = false
      result = undefined
      resultFor = ''
      refused = false
      err = undefined
      fileError = undefined
    })
  })

  const input = $derived(`${format}|${replace}|${text}`)
  const current = $derived(!!result && resultFor === input)
  const canApply = $derived(current && !refused && (result?.errors ?? []).length === 0 && !busy)

  const formatOptions = $derived<SelectOption[]>([
    { value: 'csv', label: t('dns.dhcp.import.format.csv') },
    { value: 'hosts', label: t('dns.dhcp.import.format.hosts') },
    { value: 'lines', label: t('dns.dhcp.import.format.lines') },
  ])

  const PLACEHOLDER: Record<DhcpImportFormat, string> = {
    csv: 'mac,ip,hostname,comment\naa:bb:cc:dd:ee:01,192.168.1.20,printer,Office\naa:bb:cc:dd:ee:02,192.168.1.21,nas,',
    hosts: '192.168.1.20\tprinter\t# aa:bb:cc:dd:ee:01\n192.168.1.21\tnas\t# aa:bb:cc:dd:ee:02',
    lines: 'aa:bb:cc:dd:ee:01 192.168.1.20 printer\naa:bb:cc:dd:ee:02 192.168.1.21',
  }

  const formatHelp = $derived(
    {
      csv: t('dns.dhcp.import.help.csv'),
      hosts: t('dns.dhcp.import.help.hosts'),
      lines: t('dns.dhcp.import.help.lines'),
    }[format],
  )

  const tooLarge = $derived(new Blob([text]).size > MAX_BYTES)
  const textError = $derived(fieldError(err, 'text') ?? (tooLarge ? t('dns.dhcp.import.tooLarge') : fileError))
  const generalError = $derived(err && err.field !== 'text' && err.field !== 'format' ? errorText(err) : undefined)

  async function run(dryRun: boolean) {
    if (!text.trim() || tooLarge || busy) return
    const key = input
    busy = dryRun ? 'preview' : 'apply'
    err = undefined
    try {
      const r = await api.dhcp.static.import({ format, text, replace, dryRun })
      if (!dryRun && r.applied) {
        toast.success(t('dns.dhcp.import.done', { counts: counts(r) }))
        open = false
        onimported()
        return
      }
      result = r
      resultFor = key
      refused = !dryRun
    } catch (e) {
      err = toApiError(e)
    } finally {
      busy = undefined
    }
  }

  function counts(r: DhcpImportResult): string {
    return t('dns.dhcp.import.counts', {
      added: formatNumber(r.added),
      updated: formatNumber(r.updated),
      unchanged: formatNumber(r.unchanged),
      removed: formatNumber(r.removed),
    })
  }

  async function picked() {
    const f = fileInput?.files?.[0]
    if (fileInput) fileInput.value = '' // choosing the same file again fires change again
    if (!f) return
    fileError = undefined
    if (f.size > MAX_BYTES) {
      fileError = t('dns.dhcp.import.tooLarge')
      return
    }
    const name = f.name.toLowerCase()
    if (name.endsWith('.csv')) format = 'csv'
    else if (name.endsWith('.hosts') || name === 'hosts') format = 'hosts'
    text = await f.text()
  }

  // ---- refused lines

  const FIELDS: Record<string, () => string> = {
    mac: () => t('dns.dhcp.static.mac'),
    ip: () => t('dns.dhcp.static.ip'),
    hostname: () => t('dns.dhcp.static.hostname'),
    comment: () => t('common.label.comment'),
    clientId: () => t('dns.dhcp.static.clientId'),
    leaseSeconds: () => t('dns.dhcp.static.leaseTime'),
    row: () => t('dns.dhcp.import.fieldRow'),
    text: () => t('dns.dhcp.import.fieldText'),
  }

  type Row = DhcpImportError & { i: number }
  const errorRows = $derived<Row[]>((result?.errors ?? []).map((e, i) => ({ ...e, i })))

  const columns = $derived<Column<Row>[]>([
    {
      key: 'line',
      label: t('dns.dhcp.import.line'),
      align: 'right',
      width: '1%',
      value: (e) => e.line,
      format: (e) => (e.line > 0 ? formatNumber(e.line) : t('dns.dhcp.import.whole')),
    },
    { key: 'field', label: t('dns.dhcp.import.field'), width: '1%', value: (e) => e.field, cell: fieldCell },
    { key: 'message', label: t('dns.dhcp.import.message'), value: (e) => e.message, cell: messageCell },
  ])
</script>

{#snippet fieldCell(e: Row)}
  <span class="nowrap">{FIELDS[e.field]?.() ?? e.field}</span>
{/snippet}

{#snippet messageCell(e: Row)}
  <span class="msg">{e.message}</span>
{/snippet}

<Dialog bind:open title={t('dns.dhcp.import.title')} size="lg" dismissible={busy !== 'apply'}>
  <div class="stack">
    <p class="small muted">{t('dns.dhcp.import.intro')}</p>

    <div class="row">
      <div class="format">
        <Field label={t('dns.dhcp.import.format')} error={fieldError(err, 'format')}>
          <Select bind:value={format} options={formatOptions} />
        </Field>
      </div>
      <input
        bind:this={fileInput}
        class="visually-hidden"
        type="file"
        accept=".csv,.txt,.hosts,text/csv,text/plain"
        tabindex="-1"
        aria-hidden="true"
        onchange={picked}
      />
      <Button icon="upload" onclick={() => fileInput?.click()}>{t('dns.dhcp.import.file')}</Button>
    </div>

    <Field label={t('dns.dhcp.import.text')} help={formatHelp} error={textError}>
      <Textarea bind:value={text} rows={8} mono placeholder={PLACEHOLDER[format]} autocomplete="off" autocapitalize="off" />
    </Field>

    <div class="stack-sm">
      <Checkbox bind:checked={replace} label={t('dns.dhcp.import.replace')} description={t('dns.dhcp.import.replaceHelp')} />
      {#if replace}<Notice tone="fail">{t('dns.dhcp.import.replaceWarn')}</Notice>{/if}
    </div>

    {#if generalError}<Notice tone="fail">{generalError}</Notice>{/if}

    {#if result}
      {@const bad = result.errors?.length ?? 0}
      <div class="stack-sm" aria-live="polite">
        {#if !current}
          <Notice tone="info">{t('dns.dhcp.import.changed')}</Notice>
        {:else if bad === 0}
          <Notice tone="ok" title={t('dns.dhcp.import.ok')}>{counts(result)}</Notice>
        {:else}
          <Notice tone="fail" title={refused ? t('dns.dhcp.import.refused') : tn('dns.dhcp.import.errors', bad)}>
            <p>{t('dns.dhcp.import.errorsText')}</p>
            <p class="small">{t('dns.dhcp.import.validRows', { counts: counts(result) })}</p>
          </Notice>
          <div class="errors">
            <Table
              {columns}
              rows={errorRows}
              key={(e) => e.i}
              compact
              maxHeight="16rem"
              caption={tn('dns.dhcp.import.errors', bad)}
            />
          </div>
        {/if}
      </div>
    {/if}
  </div>

  {#snippet actions()}
    {#if !canApply && text.trim()}<p class="hint small subtle">{t('dns.dhcp.import.previewFirst')}</p>{/if}
    <Button variant="ghost" disabled={busy === 'apply'} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button icon="search" loading={busy === 'preview'} disabled={!text.trim() || tooLarge || !!busy} onclick={() => run(true)}>
      {t('dns.dhcp.import.preview')}
    </Button>
    <Button variant={replace ? 'danger' : 'primary'} loading={busy === 'apply'} disabled={!canApply} onclick={() => run(false)}>
      {t('dns.dhcp.import.apply')}
    </Button>
  {/snippet}
</Dialog>

<style>
  .row {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    gap: var(--sp-3);
  }
  .format {
    flex: 0 1 280px;
    min-width: 0;
  }
  .errors {
    min-width: 0;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    overflow: hidden;
  }
  .msg {
    display: block;
    min-width: 20ch;
    overflow-wrap: anywhere;
  }
  .hint {
    flex: 1 1 100%;
    margin: 0;
    text-align: right;
  }
</style>
