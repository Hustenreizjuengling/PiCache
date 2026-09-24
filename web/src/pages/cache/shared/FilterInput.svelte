<!--
  @component
  A text filter bound to a URL parameter. It reports a new value 350 ms after
  typing stops (or on Enter), only when `valid` accepts it (e.g. at least three
  characters), and follows outside changes such as Back or "Clear filters".
  <FilterInput label="Client" value={router.param('client')} onchange={(v) => router.setQuery({ client: v })} />
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import type { IconName } from '$lib/icons'
  import { Field, Input } from '$lib/ui'

  interface Props {
    label: string
    /** Current value (from the URL). */
    value: string
    onchange: (value: string) => void
    placeholder?: string
    mono?: boolean
    icon?: IconName
    /** Values that may be sent; others show `invalidText`. */
    valid?: (value: string) => boolean
    invalidText?: string
    help?: string
    /** CSS width of the field. */
    width?: string
  }

  let {
    label,
    value,
    onchange,
    placeholder,
    mono = false,
    icon = 'search',
    valid = () => true,
    invalidText,
    help,
    width = '220px',
  }: Props = $props()

  const DEBOUNCE_MS = 350

  let text = $state(untrack(() => value))
  let timer: ReturnType<typeof setTimeout> | undefined

  // Outside changes replace the text (typing never triggers this: the value
  // only changes after onchange reported the trimmed text).
  $effect(() => {
    const v = value
    untrack(() => {
      if (v !== text.trim()) text = v
    })
  })

  $effect(() => () => clearTimeout(timer))

  function apply() {
    clearTimeout(timer)
    const v = text.trim()
    if (v !== value && valid(v)) onchange(v)
  }

  function oninput() {
    clearTimeout(timer)
    timer = setTimeout(apply, DEBOUNCE_MS)
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      apply()
    }
  }

  const invalid = $derived(!valid(text.trim()))
</script>

<div class="filter" style:width>
  <Field {label} help={invalid ? invalidText : help}>
    <Input
      type="search"
      size="sm"
      {icon}
      {mono}
      {placeholder}
      autocomplete="off"
      bind:value={text}
      {oninput}
      {onkeydown}
    />
  </Field>
</div>

<style>
  .filter {
    max-width: 100%;
    min-width: 0;
  }
</style>
