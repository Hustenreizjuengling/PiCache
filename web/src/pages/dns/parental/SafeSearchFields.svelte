<!--
  @component
  Safe search of a group: one switch per search engine and a select for
  YouTube's restricted mode (off, moderate, strict), each with a short
  explanation. Bound to the group's parental.SafeSearch draft.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import type { SafeSearch, YoutubeMode } from '$lib/api'
  import { Field, Select, Toggle } from '$lib/ui'
  import { ENGINE_NAMES, ENGINES } from './plan'

  interface Props {
    value: SafeSearch
    /** Server message for safeSearch.youtube. */
    youtubeError?: string
  }

  let { value = $bindable(), youtubeError }: Props = $props()

  const MODES: readonly YoutubeMode[] = ['off', 'moderate', 'strict']
  const switches = ENGINES.filter((e) => e !== 'youtube')
</script>

<div class="engines">
  {#each switches as e (e)}
    <div class="engine">
      <Toggle
        bind:checked={value[e]}
        label={ENGINE_NAMES[e]}
        description={t(`dns.parental.safeSearch.help.${e}` as MessageKey)}
      />
    </div>
  {/each}
  <div class="engine youtube">
    <Field label={t('dns.parental.safeSearch.youtube')} help={t('dns.parental.safeSearch.help.youtube')} error={youtubeError}>
      <Select
        bind:value={() => value.youtube, (v) => (value.youtube = MODES.includes(v as YoutubeMode) ? (v as YoutubeMode) : 'off')}
        options={MODES.map((m) => ({ value: m, label: t(`dns.parental.safeSearch.youtubeMode.${m}`) }))}
      />
    </Field>
  </div>
</div>

<style>
  .engines {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-3) var(--sp-5);
  }
  .engine {
    min-width: 0;
  }
  .youtube {
    grid-column: 1 / -1;
    max-width: 420px;
  }
  @media (max-width: 560px) {
    .engines {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
