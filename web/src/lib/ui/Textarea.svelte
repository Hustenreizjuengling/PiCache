<!--
  @component
  Multi-line text (one entry per line lists, comments).
  <Textarea bind:value={domains} rows={6} mono placeholder="one host per line" />
-->
<script lang="ts">
  import type { HTMLTextareaAttributes } from 'svelte/elements'
  import { getFieldContext } from './field'

  interface Props extends HTMLTextareaAttributes {
    value?: string
    mono?: boolean
    invalid?: boolean
  }

  let { value = $bindable(''), rows = 4, mono = false, invalid, id, class: cls, ...rest }: Props = $props()

  const field = getFieldContext()
  const isInvalid = $derived(invalid ?? field?.invalid ?? false)
</script>

<textarea
  bind:value
  {rows}
  id={id ?? field?.id}
  class={[mono && 'mono', cls]}
  aria-invalid={isInvalid || undefined}
  aria-describedby={field?.describedBy}
  required={field?.required || undefined}
  spellcheck={mono ? false : undefined}
  {...rest}
></textarea>

<style>
  textarea {
    width: 100%;
    min-width: 0;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    font-size: var(--fs-md);
    line-height: var(--lh);
    resize: vertical;
  }
  textarea.mono {
    font-size: var(--fs-sm);
  }
  textarea::placeholder {
    color: var(--text-3);
  }
  textarea:disabled {
    background: var(--surface-2);
    color: var(--text-2);
  }
  textarea[aria-invalid='true'] {
    border-color: var(--danger);
  }
  textarea:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
</style>
