<!--
  @component
  Imports conditional forwarders (admins) from "[/domain/…/]server …" lines,
  pasted or loaded from a file ("#" as the server means the default
  upstreams, "[//]" single-label names). A line whose first domain belongs
  to an existing forwarder replaces its domains and servers; other lines add
  forwarders. Preview checks the text on the server (dryRun: what it would
  add and change, and every refused line); Apply is enabled only after a
  preview of exactly this text without errors, and the server checks it
  again: if it still refuses, its errors are shown and nothing is saved.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type ForwarderImportError, type ForwarderImportResult } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatNumber } from '$lib/format'
  import { Button, Dialog, Field, Notice, Table, Textarea, toast, type Column } from '$lib/ui'

  interface Props {
    open?: boolean
    onimported: () => void
  }

  let { open = $bindable(false), onimported }: Props = $props()

  /** The server's limits for the text (256 KiB, 1024 lines). */
  const MAX_BYTES = 256 * 1024
  const MAX_LINES = 1024

  const PLACEHOLDER = '[/fritz.box/178.168.192.in-addr.arpa/]192.168.178.1\n[/corp.example/]10.0.0.53 10.0.0.54\n[/public.corp.example/]#\n[//]192.168.178.1'

  let text = $state('')
  let busy = $state<'preview' | 'apply' | undefined>(undefined)
  let result = $state.raw<ForwarderImportResult | undefined>(undefined)
  /** The text the result belongs to. */
  let resultFor = $state('')
  /** The result is the server refusing an Apply. */
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

  const current = $derived(!!result && resultFor === text)
  const canApply = $derived(current && !refused && (result?.errors ?? []).length === 0 && !busy)

  // Like the server: a trailing line break does not start another line.
  const tooLarge = $derived(new Blob([text]).size > MAX_BYTES || text.replace(/\n$/, '').split('\n').length > MAX_LINES)
  const textError = $derived(fieldError(err, 'text') ?? (tooLarge ? t('dns.forwarders.import.tooLarge') : fileError))
  const generalError = $derived(err && err.field !== 'text' ? errorText(err) : undefined)

  async function run(dryRun: boolean) {
    if (!text.trim() || tooLarge || busy) return
    const key = text
    busy = dryRun ? 'preview' : 'apply'
    err = undefined
    try {
      const r = await api.dns.forwarders.import({ text, dryRun })
      if (!dryRun && r.applied) {
        toast.success(t('dns.forwarders.import.done', { counts: counts(r) }))
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

  function counts(r: ForwarderImportResult): string {
    return t('dns.forwarders.import.counts', {
      added: formatNumber(r.added),
      updated: formatNumber(r.updated),
      unchanged: formatNumber(r.unchanged),
    })
  }

  async function picked() {
    const f = fileInput?.files?.[0]
    if (fileInput) fileInput.value = '' // choosing the same file again fires change again
    if (!f) return
    fileError = undefined
    if (f.size > MAX_BYTES) {
      fileError = t('dns.forwarders.import.tooLarge')
      return
    }
    text = await f.text()
  }

  // ---- refused lines

  const FIELDS: Record<string, () => string> = {
    syntax: () => t('dns.forwarders.import.fieldSyntax'),
    domains: () => t('dns.forwarders.domains'),
    upstreams: () => t('dns.forwarders.upstreams'),
    text: () => t('dns.forwarders.import.fieldText'),
  }

  type Row = ForwarderImportError & { i: number }
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

<Dialog bind:open title={t('dns.forwarders.import.title')} size="lg" dismissible={busy !== 'apply'}>
  <div class="stack">
    <p class="small muted">{t('dns.forwarders.import.intro')}</p>

    <div class="row">
      <input
        bind:this={fileInput}
        class="visually-hidden"
        type="file"
        accept=".conf,.txt,text/plain"
        tabindex="-1"
        aria-hidden="true"
        onchange={picked}
      />
      <Button icon="upload" onclick={() => fileInput?.click()}>{t('dns.dhcp.import.file')}</Button>
    </div>

    <Field label={t('dns.forwarders.import.text')} help={t('dns.forwarders.import.help')} error={textError}>
      <Textarea bind:value={text} rows={8} mono placeholder={PLACEHOLDER} autocomplete="off" autocapitalize="off" />
    </Field>

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
            <p class="small">{t('dns.forwarders.import.validLines', { counts: counts(result) })}</p>
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
    <Button variant="primary" loading={busy === 'apply'} disabled={!canApply} onclick={() => run(false)}>
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
