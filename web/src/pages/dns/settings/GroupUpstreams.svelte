<!--
  @component
  The upstreams of groups (DNS settings → Upstreams): each upstream list a
  group uses instead of the default upstreams (a family resolver preset or
  the group's own list), the groups that share it, its upstreams with their
  health and statistics, the clock guard and the error when the list could
  not be built (its clients get SERVFAIL). Changed on Clients & groups and
  on the parental controls page.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { ClientGroup, GroupUpstreamSet, UpstreamPreset } from '$lib/api'
  import { formatNumber, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { Badge, Chip, Notice } from '$lib/ui'
  import { groupNames } from '../shared/groups'
  import { presetName } from '../shared/resolver'

  interface Props {
    sets: readonly GroupUpstreamSet[]
    groups: readonly ClientGroup[] | undefined
    presets: readonly UpstreamPreset[] | undefined
  }

  let { sets, groups, presets }: Props = $props()

  function title(s: GroupUpstreamSet): string {
    if (!s.preset) return t('dns.settings.groupUpstreams.own')
    return presetName(s.preset, presets)
  }
</script>

<section class="stack-sm sub" aria-labelledby="dns-group-ups-title">
  <h3 id="dns-group-ups-title">{t('dns.settings.groupUpstreams.title')}</h3>
  <p class="small muted">
    {t('dns.settings.groupUpstreams.help')}
    <a href={href('/dns/clients', { tab: 'groups' })}>{t('dns.settings.groupUpstreams.open')}</a>
  </p>
  <ul class="sets">
    {#each sets as s, i (i)}
      <li>
        <div class="head">
          <span class="name">{title(s)}</span>
          <span class="small muted">{tn('dns.settings.groupUpstreams.groups', s.groupIds.length, { groups: groupNames(s.groupIds, groups) })}</span>
          {#if s.clockGuard}<Badge tone="warn" title={t('dns.settings.groupUpstreams.clockGuardHelp')}>{t('dns.settings.groupUpstreams.clockGuard')}</Badge>{/if}
        </div>
        {#if s.error}
          <Notice tone="fail" title={t('dns.settings.groupUpstreams.failed')}><span class="mono small">{s.error}</span></Notice>
        {/if}
        {#if s.upstreams.length > 0}
          <ul class="ups">
            {#each s.upstreams as u (u.upstream)}
              <li>
                <span class="mono u">{u.upstream}</span>
                <span class="meta small">
                  <Chip size="sm" tone={u.healthy ? 'ok' : 'fail'} label={u.healthy ? t('dns.settings.upstreams.healthy') : t('dns.settings.upstreams.failing')} />
                  <span class="muted">
                    {t('dns.settings.upstreams.stats', {
                      queries: tn('dns.settings.upstreams.queries', u.queries, { count: formatNumber(u.queries) }),
                      errors: tn('dns.settings.upstreams.errors', u.errors, { count: formatNumber(u.errors) }),
                      rtt: formatNumber(u.avgRttMs, 1),
                    })}
                  </span>
                  {#if u.lastError}
                    <span class="warn" title={u.lastError}>{t('dns.settings.upstreams.lastError', { when: formatRelative(u.lastErrorAt) })}</span>
                  {/if}
                </span>
              </li>
            {/each}
          </ul>
        {/if}
      </li>
    {/each}
  </ul>
</section>

<style>
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .sets,
  .ups {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .sets > li {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    min-width: 0;
  }
  .ups {
    gap: var(--sp-2);
  }
  .ups li {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  .name {
    font-weight: 600;
  }
  .u {
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
  }
  .warn {
    color: var(--warning);
  }
</style>
