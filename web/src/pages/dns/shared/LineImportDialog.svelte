<!--
  @component
  Imports lines (your rules, hosts records) pasted or loaded from a file,
  with the options of the page above the text (groups, scope). Preview checks
  the text on the server (dryRun: what it would add, what stays unchanged or
  is skipped, and the first error of each refused line); "Remove lines with
  errors" drops those lines from the text and previews again. Import is
  enabled only after a preview of exactly this text and these options
  without errors, and the server checks everything again: if it still
  refuses, its errors are shown and nothing is saved.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { toApiError, type ApiError, type ImportLineError, type LineImportResult } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatNumber } from '$lib/format'
  import { Button, Dialog, Field, Notice, Table, Textarea, toast, type Column } from '$lib/ui'

  interface Props {
    open?: boolean
    title: string
    intro: string
    textLabel: string
    help: string
    placeholder: string
    /** The server's limits for the text. */
    maxLines: number
    maxBytes?: number
    /** File types offered by "Load file". */
    accept?: string
    /** Labels of the error fields (syntax, name, …); others are shown as sent. */
    fields: Record<string, () => string>
    /** The options (serialised): a preview is current only for the same options. */
    optionsKey?: string
    /** The options allow no import (e.g. no group chosen). */
    blocked?: boolean
    /** Checks (dryRun) or applies the text. */
    run: (text: string, dryRun: boolean) => Promise<LineImportResult>
    /** The toast after an applied import. */
    doneText: (r: LineImportResult) => string
    onimported: () => void
    /** The page's options (groups, scope), above the text. */
    options?: Snippet
  }

  let {
    open = $bindable(false),
    title,
    intro,
    textLabel,
    help,
    placeholder,
    maxLines,
    maxBytes = 1024 * 1024,
    accept = '.txt,text/plain',
    fields,
    optionsKey = '',
    blocked = false,
    run,
    doneText,
    onimported,
    options,
  }: Props = $props()

  let text = $state('')
  let busy = $state<'preview' | 'apply' | undefined>(undefined)
  let result = $state.raw<LineImportResult | undefined>(undefined)
  /** The text and options the result belongs to. */
  let resultFor = $state('')
  /** The result is the server refusing an import. */
  let refused = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let fileError = $state<string | undefined>(undefined)
  let fileInput: HTMLInputElement | undefined = $state()

  $effect.pre(() => {
    if (!open) return
    untrack(() => {
      text = ''
      result = undefined
      resultFor = ''
      refused = false
      err = undefined
      fileError = undefined
    })
  })

  const key = $derived(`${optionsKey}\n${text}`)
  const current = $derived(!!result && resultFor === key)
  const canApply = $derived(current && !refused && (result?.errorCount ?? 0) === 0 && !busy && !blocked)
  const badLines = $derived([...new Set((result?.errors ?? []).map((e) => e.line).filter((l) => l > 0))])

  // Like the server: a trailing line break does not start another line.
  const lineCount = $derived(text.replace(/\r?\n$/, '').split('\n').length)
  const tooLarge = $derived(new Blob([text]).size > maxBytes || lineCount > maxLines)
  const textError = $derived(
    fieldError(err, 'text') ??
      (tooLarge ? t('dns.import.tooLarge', { lines: formatNumber(maxLines), size: formatNumber(maxBytes / 1024 / 1024) }) : fileError),
  )
  const generalError = $derived(err && err.field !== 'text' ? errorText(err) : undefined)

  async function send(dryRun: boolean) {
    if (!text.trim() || tooLarge || busy || blocked) return
    const k = key
    busy = dryRun ? 'preview' : 'apply'
    err = undefined
    try {
      const r = await run(text, dryRun)
      if (!dryRun && r.applied) {
        toast.success(doneText(r))
        open = false
        onimported()
        return
      }
      result = r
      resultFor = k
      refused = !dryRun
    } catch (e) {
      err = toApiError(e)
    } finally {
      busy = undefined
    }
  }

  /** Drops the lines the preview refused and checks the rest again. */
  function removeBad() {
    const bad = new Set(badLines)
    if (bad.size === 0) return
    text = text
      .split('\n')
      .filter((_, i) => !bad.has(i + 1))
      .join('\n')
    void send(true)
  }

  function counts(r: LineImportResult): string {
    return t('dns.import.counts', {
      added: formatNumber(r.added),
      unchanged: formatNumber(r.unchanged),
      skipped: formatNumber(r.skipped),
    })
  }

  async function picked() {
    const f = fileInput?.files?.[0]
    if (fileInput) fileInput.value = '' // choosing the same file again fires change again
    if (!f) return
    fileError = undefined
    if (f.size > maxBytes) {
      fileError = t('dns.import.tooLarge', { lines: formatNumber(maxLines), size: formatNumber(maxBytes / 1024 / 1024) })
      return
    }
    text = await f.text()
  }

  // ---- refused lines

  type Row = ImportLineError & { i: number }
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
  <span class="nowrap">{fields[e.field]?.() ?? (e.field === 'text' ? t('dns.dhcp.import.fieldText') : e.field)}</span>
{/snippet}

{#snippet messageCell(e: Row)}
  <span class="msg">{e.message}</span>
{/snippet}

<Dialog bind:open {title} size="lg" dismissible={busy !== 'apply'}>
  <div class="stack">
    <p class="small muted">{intro}</p>

    {@render options?.()}

    <div class="row">
      <input
        bind:this={fileInput}
        class="visually-hidden"
        type="file"
        {accept}
        tabindex="-1"
        aria-hidden="true"
        onchange={picked}
      />
      <Button icon="upload" onclick={() => fileInput?.click()}>{t('dns.dhcp.import.file')}</Button>
    </div>

    <Field label={textLabel} {help} error={textError}>
      <Textarea bind:value={text} rows={8} mono {placeholder} autocomplete="off" autocapitalize="off" />
    </Field>

    {#if generalError}<Notice tone="fail">{generalError}</Notice>{/if}

    {#if result}
      <div class="stack-sm" aria-live="polite">
        {#if !current}
          <Notice tone="info">{t('dns.dhcp.import.changed')}</Notice>
        {:else if result.errorCount === 0}
          <Notice tone="ok" title={t('dns.dhcp.import.ok')}>{counts(result)}</Notice>
        {:else}
          <Notice tone="fail" title={refused ? t('dns.dhcp.import.refused') : tn('dns.dhcp.import.errors', result.errorCount)}>
            <p>{t('dns.dhcp.import.errorsText')}</p>
            <p class="small">{t('dns.import.validLines', { counts: counts(result) })}</p>
            {#if result.errorCount > result.errors.length}
              <p class="small">{t('dns.import.firstErrors', { shown: formatNumber(result.errors.length), count: formatNumber(result.errorCount) })}</p>
            {/if}
            {#snippet actions()}
              {#if badLines.length > 0}
                <Button size="sm" icon="trash" disabled={!!busy} onclick={removeBad}>
                  {tn('dns.import.removeBad', badLines.length)}
                </Button>
              {/if}
            {/snippet}
          </Notice>
          <div class="errors">
            <Table
              {columns}
              rows={errorRows}
              key={(e) => e.i}
              compact
              maxHeight="16rem"
              caption={tn('dns.dhcp.import.errors', result.errorCount)}
            />
          </div>
        {/if}
      </div>
    {/if}
  </div>

  {#snippet actions()}
    {#if !canApply && text.trim()}<p class="hint small subtle">{t('dns.import.previewFirst')}</p>{/if}
    <Button variant="ghost" disabled={busy === 'apply'} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button icon="search" loading={busy === 'preview'} disabled={!text.trim() || tooLarge || blocked || !!busy} onclick={() => send(true)}>
      {t('dns.dhcp.import.preview')}
    </Button>
    <Button variant="primary" loading={busy === 'apply'} disabled={!canApply} onclick={() => send(false)}>
      {t('dns.import.apply')}
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
