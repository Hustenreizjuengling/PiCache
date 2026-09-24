<!--
  @component
  A four-bar strength hint for a new password (guidance only; the server
  enforces the minimum length).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { passwordStrength } from '../../auth/strength'
  import { MIN_PASSWORD } from '../forms'

  let { password }: { password: string } = $props()

  const strength = $derived(passwordStrength(password, MIN_PASSWORD))
  const label = $derived(
    [
      t('auth.setup.strength.tooShort', { min: MIN_PASSWORD }),
      t('auth.setup.strength.weak'),
      t('auth.setup.strength.fair'),
      t('auth.setup.strength.good'),
      t('auth.setup.strength.strong'),
    ][strength],
  )
</script>

<div class="strength" aria-live="polite">
  <span class="bars" aria-hidden="true">
    {#each [1, 2, 3, 4] as i (i)}
      <span class={['bar', strength >= i && `s${strength}`]}></span>
    {/each}
  </span>
  <span class="small">{label}</span>
</div>

<style>
  .strength {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    margin-top: calc(-1 * var(--sp-2));
  }
  .bars {
    display: inline-flex;
    gap: 4px;
  }
  .bar {
    width: 28px;
    height: 6px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
  }
  .s1 {
    background: var(--fail);
  }
  .s2 {
    background: var(--warn);
  }
  .s3,
  .s4 {
    background: var(--ok);
  }
</style>
