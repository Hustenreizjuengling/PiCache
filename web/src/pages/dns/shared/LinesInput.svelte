<!--
  @component
  A textarea edited as a list: one entry per line, bound to a string[]
  (empty lines and duplicates are dropped). Inside a Field it picks up the
  field's id and error state like Textarea.

  <Field label="Bootstrap servers" error={form.error('bootstrap')}>
    <LinesInput bind:value={form.draft.bootstrap} placeholder="9.9.9.9" />
  </Field>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { Textarea } from '$lib/ui'
  import { lines, sameList } from './input'

  interface Props {
    value?: string[]
    rows?: number
    mono?: boolean
    placeholder?: string
    disabled?: boolean
    invalid?: boolean
    /** Accessible name when the textarea is not inside a Field. */
    ariaLabel?: string
  }

  let { value = $bindable([]), rows = 4, mono = true, placeholder, disabled, invalid, ariaLabel }: Props = $props()

  let text = $state(untrack(() => value.join('\n')))

  // Outside changes (revert, reset to default, "Exempt") replace the text;
  // our own edits already match and keep the cursor where it is.
  $effect(() => {
    const v = [...value]
    untrack(() => {
      if (!sameList(lines(text), v)) text = v.join('\n')
    })
  })

  // Reads the element's value: the binding may not have updated `text` yet.
  function oninput(e: Event & { currentTarget: HTMLTextAreaElement }) {
    const next = lines(e.currentTarget.value)
    if (!sameList(next, value)) value = next
  }
</script>

<Textarea
  bind:value={text}
  {rows}
  {mono}
  {placeholder}
  {disabled}
  {invalid}
  aria-label={ariaLabel}
  autocomplete="off"
  autocapitalize="off"
  {oninput}
/>
