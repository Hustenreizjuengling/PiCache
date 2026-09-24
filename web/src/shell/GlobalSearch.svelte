<!--
  @component
  Global search: an IP address opens the client (#/dns/clients?ip=…),
  anything else searches the query log (#/dns/queries?domain=…).
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { router } from '../lib/router.svelte'
  import { Input, toast } from '../lib/ui'

  let value = $state('')

  function isIPv4(s: string): boolean {
    const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s)
    return !!m && m.slice(1).every((p) => Number(p) <= 255)
  }

  function isIPv6(s: string): boolean {
    if (!s.includes(':') || !/^[0-9a-f:.]+$/i.test(s)) return false
    try {
      new URL(`http://[${s}]/`)
      return true
    } catch {
      return false
    }
  }

  function submit(e: SubmitEvent) {
    e.preventDefault()
    const q = value.trim().toLowerCase().replace(/\.$/, '')
    if (!q) return
    if (isIPv4(q) || isIPv6(q)) {
      router.navigate('/dns/clients', { ip: q })
    } else if (q.length < 3) {
      toast.info(t('common.search.tooShort'))
      return
    } else {
      router.navigate('/dns/queries', { domain: q })
    }
    value = ''
  }
</script>

<form class="search" role="search" onsubmit={submit}>
  <Input
    type="search"
    icon="search"
    size="sm"
    bind:value
    maxlength={253}
    placeholder={t('common.search.placeholder')}
    aria-label={t('common.search.label')}
    autocomplete="off"
  />
</form>

<style>
  .search {
    width: 240px;
    max-width: 100%;
  }
  @media (max-width: 640px) {
    .search {
      width: 100%;
    }
  }
</style>
