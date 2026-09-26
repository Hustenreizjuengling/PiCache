<!--
  @component
  An address field that also accepts "this server's address" (the value
  "self"): the address PiCache gives the asking device for its own names,
  so a blocked name can lead to PiCache (DNS settings → Blocking, rule
  replies). Bound to the string sent to the server ("", an address or
  "self").
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { SELF_ADDRESS } from '$lib/api'
  import { Checkbox, Field, Input } from '$lib/ui'

  interface Props {
    value: string
    label: string
    placeholder?: string
    help?: string
    error?: string
    required?: boolean
    optional?: boolean
  }

  let { value = $bindable(), label, placeholder, help, error, required = false, optional = false }: Props = $props()

  const self = $derived(value === SELF_ADDRESS)
  /** The address typed before "this server" was chosen (restored when it is unchecked). */
  let typed = ''

  function setSelf(on: boolean) {
    if (on) {
      typed = value
      value = SELF_ADDRESS
    } else {
      value = typed
    }
  }
</script>

<div class="addr">
  <Field {label} {help} {error} {required} {optional}>
    <Input
      bind:value={() => (self ? '' : value), (v) => (value = String(v ?? ''))}
      mono
      placeholder={self ? t('dns.shared.selfAddress') : placeholder}
      disabled={self}
      maxlength={64}
      autocomplete="off"
    />
  </Field>
  <!-- Named after the field: blocking settings and rule replies show one per address family. -->
  <Checkbox checked={self} label={t('dns.shared.selfAddress')} ariaLabel={`${label}: ${t('dns.shared.selfAddress')}`} onchange={setSelf} />
</div>

<style>
  .addr {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
  }
</style>
