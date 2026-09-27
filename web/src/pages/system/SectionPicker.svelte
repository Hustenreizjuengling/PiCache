<!--
  @component
  Checkboxes for configuration sections (a partial restore, a follower's
  synced sections) with the dependency rule: choosing clients and groups
  chooses the sections that refer to the groups too; leaving one of those
  out leaves clients and groups out as well.

  <SectionPicker all={RESTORE_SECTIONS} bind:value={sections} legend="Sections" error={…} />
-->
<script lang="ts" generics="S extends ConfigSection">
  import { t } from '$i18n/index.svelte'
  import type { ConfigSection } from '$lib/api'
  import { Checkbox } from '$lib/ui'
  import { toggleSection } from './sections'

  interface Props {
    all: readonly S[]
    value: S[]
    legend: string
    disabled?: boolean
    error?: string
  }

  let { all, value = $bindable(), legend, disabled = false, error }: Props = $props()

  const auto = $props.id()
</script>

<fieldset class="picker" {disabled} aria-describedby="sections-rule-{auto}{error ? ` sections-err-${auto}` : ''}">
  <legend>{legend}</legend>
  <div class="list">
    {#each all as s (s)}
      <Checkbox
        checked={value.includes(s)}
        label={t(`system.section.${s}`)}
        description={t(`system.section.${s}Help`)}
        onchange={(on) => (value = toggleSection(all, value, s, on))}
      />
    {/each}
  </div>
  <p id="sections-rule-{auto}" class="small muted">{t('system.section.rule')}</p>
  {#if error}<p id="sections-err-{auto}" class="err">{error}</p>{/if}
</fieldset>

<style>
  .picker {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  legend {
    margin-bottom: var(--sp-2);
    padding: 0;
    font-weight: 600;
    font-size: var(--fs-sm);
  }
  .list {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
</style>
