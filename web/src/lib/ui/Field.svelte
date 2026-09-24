<!--
  @component
  Label above the control, validation error and help text below. The control
  inside (Input, Select, Textarea) gets id, aria-describedby and aria-invalid.
  <Field label="Name" help="Shown in the query log." error={errors.name}>
    <Input bind:value={name} />
  </Field>
  For custom controls use the context: getFieldContext() from lib/ui/field.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import { setFieldContext } from './field'
  import Icon from './Icon.svelte'

  interface Props {
    label: string
    help?: string
    /** Validation message (e.g. ApiError.message when ApiError.field names this field). */
    error?: string | null
    required?: boolean
    /** Adds "(optional)" to the label. */
    optional?: boolean
    /** Hide the label visually (still read by screen readers). */
    hideLabel?: boolean
    id?: string
    children: Snippet
  }

  let { label, help, error, required = false, optional = false, hideLabel = false, id, children }: Props = $props()

  const auto = $props.id()
  const controlId = $derived(id ?? `field-${auto}`)
  const describedBy = $derived(
    [error ? `${controlId}-error` : '', help ? `${controlId}-help` : ''].filter(Boolean).join(' ') || undefined,
  )

  setFieldContext({
    get id() {
      return controlId
    },
    get describedBy() {
      return describedBy
    },
    get invalid() {
      return !!error
    },
    get required() {
      return required
    },
  })
</script>

<div class="field">
  <label class={['label', hideLabel && 'visually-hidden']} for={controlId}>
    {label}{#if optional}{' '}<span class="opt">({t('common.field.optional')})</span>{/if}
  </label>
  {@render children()}
  {#if error}
    <p class="error" id="{controlId}-error"><Icon name="alert" size={16} />{error}</p>
  {/if}
  {#if help}
    <p class="help" id="{controlId}-help">{help}</p>
  {/if}
</div>

<style>
  .field {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
  }
  .label {
    font-size: var(--fs-sm);
    font-weight: 600;
    color: var(--text);
  }
  .opt {
    font-weight: 400;
    color: var(--text-3);
  }
  .help {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .error {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    font-size: var(--fs-sm);
    color: var(--danger);
  }
  .error :global(.icon) {
    margin-top: 1px;
  }
</style>
