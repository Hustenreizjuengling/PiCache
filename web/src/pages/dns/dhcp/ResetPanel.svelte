<!--
  @component
  Resetting the DHCP server (admins, where the host allows destructive
  actions; works while the server is unavailable too):
  "End all leases" (DELETE /dhcp/leases: every handed-out address, also on
  reserved addresses) and "Reset DHCP" (POST /dhcp/reset: settings back to
  their defaults, which switches the server off, and every reservation and
  lease deleted), each behind a confirmation that says what is deleted.
  Other DHCP servers seen and the recent exchanges are kept.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api } from '$lib/api'
  import { Button, confirm, Panel, toast } from '$lib/ui'

  interface Props {
    /** The settings below have unsaved changes (a reset would discard them). */
    dirty: boolean
    onleasesended: () => void
    onreset: () => void
  }

  let { dirty, onleasesended, onreset }: Props = $props()

  async function endAll() {
    let deleted = 0
    const ok = await confirm({
      title: t('dns.dhcp.reset.leasesTitle'),
      message: t('dns.dhcp.reset.leasesConfirm'),
      confirmLabel: t('dns.dhcp.reset.leases'),
      action: async () => {
        deleted = (await api.dhcp.endAllLeases()).deleted
      },
    })
    if (!ok) return
    toast.success(tn('dns.dhcp.reset.leasesDone', deleted))
    onleasesended()
  }

  async function resetAll() {
    const ok = await confirm({
      title: t('dns.dhcp.reset.allTitle'),
      message: t('dns.dhcp.reset.allConfirm'),
      confirmLabel: t('dns.dhcp.reset.all'),
      action: () => api.dhcp.reset(),
    })
    if (!ok) return
    toast.success(t('dns.dhcp.reset.allDone'))
    onreset()
  }
</script>

<Panel id="dhcp-reset" title={t('dns.dhcp.reset.title')} description={t('dns.dhcp.reset.description')}>
  <div class="rows">
    <div class="row">
      <div class="text">
        <h3>{t('dns.dhcp.reset.leases')}</h3>
        <p class="small muted">{t('dns.dhcp.reset.leasesText')}</p>
      </div>
      <Button icon="trash" onclick={endAll}>{t('dns.dhcp.reset.leases')}</Button>
    </div>
    <div class="row">
      <div class="text">
        <h3>{t('dns.dhcp.reset.all')}</h3>
        <p class="small muted">{t('dns.dhcp.reset.allText')}</p>
        {#if dirty}<p class="small subtle">{t('dns.dhcp.hint.saveFirst')}</p>{/if}
      </div>
      <Button variant="danger" icon="refresh" disabled={dirty} onclick={resetAll}>{t('dns.dhcp.reset.all')}</Button>
    </div>
  </div>
</Panel>

<style>
  .rows {
    display: flex;
    flex-direction: column;
  }
  .row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-4);
    padding: var(--sp-3) 0;
  }
  .row:first-child {
    padding-top: 0;
  }
  .row:last-child {
    padding-bottom: 0;
  }
  .row + .row {
    border-top: 1px solid var(--line);
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    flex: 1 1 320px;
    min-width: 0;
    max-width: 90ch;
  }
  h3 {
    font-size: var(--fs-md);
  }
</style>
