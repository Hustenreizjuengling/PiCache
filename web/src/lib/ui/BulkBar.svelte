<!--
  @component
  Actions for the rows checked in a Table: the count, "clear the selection"
  and one button per action. It sticks to the bottom of the window while its
  table is in view; on narrow screens the actions move into a menu. Shown
  only while rows are checked. Deleting should ask first, naming the count.

  <BulkBar count={checked.length} onclear={() => (checked = [])} busy={busy}
           actions={[{ label: 'Enable', icon: 'play', onselect: () => run('enable') }, …]} />
-->
<script lang="ts">
  import { t, tn } from '../../i18n/index.svelte'
  import Button from './Button.svelte'
  import Icon from './Icon.svelte'
  import IconButton from './IconButton.svelte'
  import Menu from './Menu.svelte'
  import Spinner from './Spinner.svelte'
  import type { BulkAction, MenuItem } from './types'

  interface Props {
    count: number
    actions: BulkAction[]
    /** An action runs: the actions are disabled. */
    busy?: boolean
    /** A warning about the selection (e.g. what deleting also switches off). */
    note?: string
    onclear: () => void
  }

  let { count, actions, busy = false, note, onclear }: Props = $props()

  const items = $derived<MenuItem[]>(
    actions.map((a) => ({ label: a.label, icon: a.icon, danger: a.danger, disabled: busy || a.disabled, onselect: a.onselect })),
  )
</script>

{#if count > 0}
  <div class="bulk" role="region" aria-label={t('common.bulk.label')}>
    <div class="line">
      <span class="count" aria-live="polite">
        {#if busy}<Spinner size={14} />{/if}
        {tn('common.bulk.selected', count)}
      </span>
      <IconButton icon="close" size="sm" label={t('common.bulk.clear')} disabled={busy} onclick={onclear} />
      <span class="spacer"></span>
      <span class="wide">
        {#each actions as a (a.label)}
          <span title={a.title}>
            <Button size="sm" variant={a.danger ? 'danger' : 'secondary'} icon={a.icon} disabled={busy || a.disabled} onclick={a.onselect}>
              {a.label}
            </Button>
          </span>
        {/each}
      </span>
      <span class="narrow">
        <Menu label={t('common.bulk.actions')} size="sm" align="end" {items} disabled={busy} />
      </span>
    </div>
    {#if note}
      <p class="note"><Icon name="alert" size={16} /><span>{note}</span></p>
    {/if}
  </div>
{/if}

<style>
  .bulk {
    position: sticky;
    bottom: var(--sp-3);
    z-index: 4;
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: var(--sp-3);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    box-shadow: var(--shadow-float);
  }
  .line {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    min-width: 0;
  }
  .count {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    font-size: var(--fs-sm);
    font-weight: 600;
    white-space: nowrap;
  }
  .spacer {
    flex: 1;
  }
  .wide {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: var(--sp-2);
  }
  .narrow {
    display: none;
  }
  .note {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    color: var(--warning);
    font-size: var(--fs-sm);
  }
  .note :global(.icon) {
    flex: none;
    margin-top: 2px;
  }
  @media (max-width: 600px) {
    .wide {
      display: none;
    }
    .narrow {
      display: inline-flex;
    }
  }
</style>
