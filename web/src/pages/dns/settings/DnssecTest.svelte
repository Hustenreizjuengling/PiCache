<!--
  @component
  "Test DNSSEC" (admins; the route stores nothing, so it stays usable while
  the host locks the configuration): probes the default upstreams now and
  validates fixed test names through them, in every mode, so it also shows
  whether Validate would work. Disabled while the page has unsaved changes
  (the test uses the saved settings). The result stays until the page is
  left: per name the expected and the found status, the verdict, the reason
  or extended DNS error and whether the upstream's own validator refuses the
  name too; the probed upstreams; a summary with the cause of inconclusive
  checks. 409 (a test is running) and 429 (one start per 10 s) are toasts.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, isApiError, type DnssecTest, type DnssecTestCheck } from '$lib/api'
  import { formatDuration } from '$lib/format'
  import { Button, Icon, Notice, toast } from '$lib/ui'
  import { edeText } from '../querylog/ede'
  import { dnssecStatusLabel, timeReasonText } from '../shared/dnssec'
  import DnssecStatusChip from '../shared/DnssecStatusChip.svelte'
  import UpstreamDnssecChip from '../shared/UpstreamDnssecChip.svelte'

  let { dirty }: { dirty: boolean } = $props()

  let running = $state(false)
  let result = $state.raw<DnssecTest | undefined>(undefined)

  async function run() {
    running = true
    try {
      result = await api.upstreams.testDnssec()
    } catch (e) {
      // The server's messages of 409 and 429, translated (errorText would call a 429 generic).
      if (isApiError(e, 'conflict')) toast.error(t('dns.settings.dnssec.test.running'))
      else if (isApiError(e, 'too_many_requests')) toast.error(t('dns.settings.dnssec.test.tooSoon'))
      else toast.error(e)
    } finally {
      running = false
    }
  }

  const VERDICT_ICON = { pass: 'success', fail: 'error', inconclusive: 'alert' } as const

  function verdictLabel(v: DnssecTestCheck['verdict']): string {
    return {
      pass: t('dns.settings.dnssec.test.verdict.pass'),
      fail: t('dns.settings.dnssec.test.verdict.fail'),
      inconclusive: t('dns.settings.dnssec.test.verdict.inconclusive'),
    }[v]
  }

  /** The reason, or the extended DNS error when it carries the reason (its text is "<zone>: <reason>"). */
  function detail(c: DnssecTestCheck): string {
    if (c.ede && c.reason && (c.ede.text === c.reason || c.ede.text.endsWith(`: ${c.reason}`))) return edeText(c.ede)
    if (c.reason) return c.reason
    if (c.ede) return edeText(c.ede)
    return c.rcode && c.rcode !== 'NOERROR' ? c.rcode : ''
  }

  /** Why checks were inconclusive: the clock, upstreams without DNSSEC data, names that are gone, no reply. */
  function causes(r: DnssecTest): string[] {
    const out: string[] = []
    if (r.timeChecks === 'suspended') out.push(t('dns.settings.dnssec.test.causeClock', { reason: timeReasonText(r.timeReason) }))
    const name = (u: { name: string; upstream: string }) => u.name || u.upstream
    if (r.upstreams.some((u) => u.dnssec === 'anchor-mismatch')) out.push(t('dns.settings.dnssec.test.causeAnchors'))
    const noData = r.upstreams.filter((u) => u.dnssec === 'no-dnssec').map(name)
    if (noData.length > 0) out.push(t('dns.settings.dnssec.test.causeNoData', { upstreams: noData.join(', ') }))
    const open = r.checks.filter((c) => c.verdict === 'inconclusive')
    const gone = open.filter((c) => c.status === 'error' && c.rcode === 'NXDOMAIN').map((c) => c.name)
    if (gone.length > 0) out.push(t('dns.settings.dnssec.test.causeGone', { names: gone.join(', ') }))
    if (out.length === 0 && open.length > 0) out.push(t('dns.settings.dnssec.test.causeNoReply', { names: open.map((c) => c.name).join(', ') }))
    return out
  }

  const summary = $derived.by((): { tone: 'ok' | 'warn' | 'fail'; title: string; lines: string[] } | undefined => {
    const r = result
    if (!r) return undefined
    const verdicts = r.checks.map((c) => c.verdict)
    if (verdicts.includes('fail')) return { tone: 'fail', title: t('dns.settings.dnssec.test.failTitle'), lines: [] }
    if (verdicts.length === 0 || verdicts.includes('inconclusive')) {
      return { tone: 'warn', title: t('dns.settings.dnssec.test.inconclusiveTitle'), lines: causes(r) }
    }
    return {
      tone: 'ok',
      title: t('dns.settings.dnssec.test.passTitle'),
      lines: r.mode === 'validate' ? [] : [t('dns.settings.dnssec.test.canSwitch')],
    }
  })
</script>

<section class="stack-sm sub" aria-labelledby="dns-dnssec-test">
  <h3 id="dns-dnssec-test">{t('dns.settings.dnssec.test.title')}</h3>
  <p class="small muted">{t('dns.settings.dnssec.test.help')}</p>
  <div class="row">
    <Button icon="shield-check" loading={running} disabled={dirty || running} onclick={run}>{t('dns.settings.dnssec.test.run')}</Button>
    {#if dirty}<span class="small subtle">{t('dns.settings.dnssec.test.saveFirst')}</span>{/if}
  </div>

  {#if result && summary}
    <div class="stack-sm" aria-live="polite">
      <Notice tone={summary.tone} title={summary.title}>
        {#if summary.lines.length === 1}
          {summary.lines[0]}
        {:else if summary.lines.length > 1}
          <ul>
            {#each summary.lines as l, i (i)}<li>{l}</li>{/each}
          </ul>
        {/if}
      </Notice>

      <ul class="checks">
        {#each result.checks as c (c.name)}
          <li>
            <span class={['verdict', c.verdict]} title={verdictLabel(c.verdict)}>
              <Icon name={VERDICT_ICON[c.verdict]} size={18} />
              <span class="visually-hidden">{verdictLabel(c.verdict)}</span>
            </span>
            <div class="body">
              <div class="line">
                <span class="mono name">{c.name}</span>
                <DnssecStatusChip status={c.status} />
                <span class="small muted">{t('dns.settings.dnssec.test.expected', { status: dnssecStatusLabel(c.expect) })}</span>
              </div>
              {#if detail(c)}<p class="small muted detail">{detail(c)}</p>{/if}
              {#if c.upstreamRefused}<p class="small">{t('dns.settings.dnssec.test.upstreamRefused')}</p>{/if}
            </div>
          </li>
        {/each}
      </ul>

      {#if result.upstreams.length > 0}
        <section class="stack-sm" aria-labelledby="dns-dnssec-test-ups">
          <h4 id="dns-dnssec-test-ups">{t('dns.settings.dnssec.test.upstreams')}</h4>
          <ul class="ups">
            {#each result.upstreams as u (u.upstream)}
              <li>
                <span class="mono">{u.name || u.upstream}</span>
                {#if u.dnssec}<UpstreamDnssecChip state={u.dnssec} />{/if}
                {#if u.dnssecError}<span class="small muted">{u.dnssecError}</span>{/if}
              </li>
            {/each}
          </ul>
        </section>
      {/if}
      <p class="small subtle">{t('dns.settings.dnssec.test.took', { duration: formatDuration(result.durationMs) })}</p>
    </div>
  {/if}
</section>

<style>
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  h4 {
    font-size: var(--fs-sm);
    font-weight: 600;
    color: var(--text-2);
  }
  .checks,
  .ups {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .checks li {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-2) 0;
    border-bottom: 1px solid var(--line);
    min-width: 0;
  }
  .checks li:last-child {
    border-bottom: 0;
  }
  .verdict {
    display: inline-flex;
    flex: none;
    margin-top: 2px;
  }
  .verdict.pass {
    color: var(--ok);
  }
  .verdict.fail {
    color: var(--fail);
  }
  .verdict.inconclusive {
    color: var(--warn);
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: 2px;
    flex: 1;
    min-width: 0;
  }
  .line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  .name {
    font-weight: 600;
  }
  .detail {
    overflow-wrap: anywhere;
  }
  .ups li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    font-size: var(--fs-sm);
    min-width: 0;
  }
</style>
