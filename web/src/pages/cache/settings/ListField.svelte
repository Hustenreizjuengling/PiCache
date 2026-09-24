<!--
  @component
  A list setting edited as text, one entry per line (commas and spaces also
  separate). `values` is updated while typing; the text keeps the user's
  layout until the list changes from outside (load, discard, save).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { Field, Textarea } from '$lib/ui'
  import { parseList, sameList } from '../shared/util'

  interface Props {
    values: string[]
    label: string
    help?: string
    error?: string
    placeholder?: string
    rows?: number
    disabled?: boolean
  }

  let { values = $bindable([]), label, help, error, placeholder, rows = 3, disabled = false }: Props = $props()

  let text = $state(untrack(() => values.join('\n')))

  $effect(() => {
    const v = values
    untrack(() => {
      if (!sameList(parseList(text), v)) text = v.join('\n')
    })
  })

  function set(v: string | undefined) {
    text = v ?? ''
    const list = parseList(text)
    if (!sameList(list, values)) values = list
  }
</script>

<Field {label} {help} {error} optional>
  <Textarea bind:value={() => text, set} {rows} mono {placeholder} {disabled} />
</Field>
