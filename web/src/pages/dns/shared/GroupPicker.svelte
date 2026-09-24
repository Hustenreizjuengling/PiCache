<!--
  @component
  Chooses the groups of a list, rule or client (checkboxes, bound to the
  sorted group ids). Disabled groups are marked; an empty selection gets a
  warning, because entries without a group apply to nobody.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ClientGroup } from '$lib/api'
  import { Badge, Checkbox, Icon, Skeleton } from '$lib/ui'
  import { normalizeIds } from './groups'

  interface Props {
    groups: readonly ClientGroup[] | undefined
    value?: number[]
    label?: string
    help?: string
    error?: string
    disabled?: boolean
    /** Text shown when nothing is selected (default: "applies to nobody"). */
    emptyWarning?: string
  }

  let { groups, value = $bindable([]), label, help, error, disabled = false, emptyWarning }: Props = $props()

  const auto = $props.id()

  function toggle(id: number, on: boolean) {
    value = normalizeIds(on ? [...value, id] : value.filter((x) => x !== id))
  }
</script>

<fieldset class="picker" aria-describedby={[error && `gp-${auto}-err`, help && `gp-${auto}-help`].filter(Boolean).join(' ') || undefined}>
  <legend>{label ?? t('common.label.groups')}</legend>
  {#if !groups}
    <Skeleton height="64px" />
  {:else}
    <div class="list">
      {#each groups as g (g.id)}
        <div class="item">
          <Checkbox checked={value.includes(g.id)} label={g.name} {disabled} onchange={(on) => toggle(g.id, on)} />
          {#if !g.enabled}<Badge tone="warn">{t('dns.shared.groupDisabled')}</Badge>{/if}
        </div>
      {/each}
    </div>
  {/if}
  {#if error}
    <p class="error" id="gp-{auto}-err"><Icon name="alert" size={16} />{error}</p>
  {:else if groups && value.length === 0}
    <p class="warn"><Icon name="alert" size={16} />{emptyWarning ?? t('dns.shared.noGroupWarning')}</p>
  {/if}
  {#if help}<p class="help" id="gp-{auto}-help">{help}</p>{/if}
</fieldset>

<style>
  .picker {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 6px;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .list {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    max-height: 220px;
    overflow: auto;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .item {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
  }
  .help {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .error,
  .warn {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    font-size: var(--fs-sm);
  }
  .error {
    color: var(--danger);
  }
  .warn {
    color: var(--warning);
  }
</style>
