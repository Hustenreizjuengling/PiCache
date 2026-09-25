<!--
  @component
  The addresses of one device (a phone with an IPv4 and several changing
  IPv6 addresses): the first one and "+N addresses", which expands to the
  others and lists all of them in its tooltip. The button keeps a table
  row's click for itself.
  <AddressList addresses={['192.168.1.20', 'fd00::1c2', '2001:db8::1c2']} />
  <AddressList addresses={item.addresses} first={item.key} hideFirst />
  <AddressList addresses={extra} first="" />  (only "+N addresses")
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'

  interface Props {
    addresses: readonly string[]
    /** The address shown first (default: the first of `addresses`; '' shows none, only "+N addresses"). */
    first?: string
    /** Leave the first address out (it is shown elsewhere, e.g. as the row's main text). */
    hideFirst?: boolean
  }

  let { addresses, first, hideFirst = false }: Props = $props()

  let open = $state(false)

  const head = $derived(first ?? addresses[0] ?? '')
  const rest = $derived(addresses.filter((a) => a !== head))
</script>

<span class="addrs">
  {#if head && !hideFirst}<span class="addr mono">{head}</span>{/if}
  {#if open}
    {#each rest as a (a)}<span class="addr mono">{a}</span>{/each}
  {/if}
  {#if rest.length > 0}
    <button type="button" class="more" aria-expanded={open} title={[head, ...rest].filter(Boolean).join('\n')} onclick={() => (open = !open)}>
      {open ? t('dns.shared.fewerAddresses') : tn('dns.shared.moreAddresses', rest.length)}
    </button>
  {/if}
</span>

<style>
  .addrs {
    display: inline-flex;
    flex-direction: column;
    align-items: flex-start;
    min-width: 0;
    max-width: 100%;
    line-height: 1.35;
  }
  /* A split IPv6 address is easily misread: tables scroll sideways instead. */
  .addr {
    white-space: nowrap;
  }
  .more {
    padding: 0;
    border: 0;
    background: none;
    color: var(--text-3);
    font: inherit;
    font-family: var(--font);
    font-size: var(--fs-xs);
    text-decoration: underline dotted;
    text-underline-offset: 3px;
    white-space: nowrap;
    cursor: pointer;
  }
  .more:hover {
    color: var(--text);
  }
  .more:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
    border-radius: 2px;
  }
</style>
