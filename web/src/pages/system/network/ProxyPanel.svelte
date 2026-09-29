<!--
  @component
  The outbound proxy (settings.network): its URL (http:// or socks5://),
  user name and write-only password (kept while the proxy and user name
  stay the same; "Remove" deletes it), and what goes through it: list
  downloads, the release check, notifications. Explains what the proxy can
  see and that its credentials cross the network unencrypted; DNS
  upstreams, the download cache, the follower sync and WHOIS lookups never
  use it, update installs use the host's PICACHE_UPDATE_PROXY.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { NetworkSettings } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Checkbox, Field, Input, Notice, Panel, Skeleton, toast, Toggle } from '$lib/ui'

  const form = settingsForm('network')

  let password = $state('')
  let removePassword = $state(false)

  const d = $derived(form.draft as NetworkSettings | undefined)
  const s = $derived(form.saved)
  const stored = $derived(!!s?.proxy.passwordSet)
  /** The stored password belongs to another proxy or user name once these change: it must be entered again. */
  const rebound = $derived(!!d && !!s && stored && (d.proxy.url.trim() !== s.proxy.url || d.proxy.username !== s.proxy.username))
  const dirty = $derived(form.dirty || !!password || removePassword)
  const general = $derived(form.saveError && !form.saveError.field ? form.errorMessage : undefined)
  const anyUse = $derived(!!d && (d.proxyFor.lists || d.proxyFor.updateCheck || d.proxyFor.notifications))

  async function save(e: SubmitEvent) {
    e.preventDefault()
    const draft = form.draft
    if (!draft) return
    draft.proxy.url = draft.proxy.url.trim()
    // Write-only: absent keeps the stored password, '' removes it.
    if (removePassword) draft.proxy.password = ''
    else if (password) draft.proxy.password = password
    const ok = await form.save()
    if (ok) {
      password = ''
      removePassword = false
      toast.success(t('common.state.saved'))
    } else if (form.draft) {
      delete form.draft.proxy.password
    }
  }

  function discard() {
    password = ''
    removePassword = false
    form.revert()
  }

  const formId = $props.id()
</script>

{#snippet footer()}
  <div class="row">
    <Button type="submit" form="proxy-{formId}" variant="primary" loading={form.saving} disabled={!dirty}>{t('common.action.save')}</Button>
    <Button variant="ghost" disabled={!dirty || form.saving} onclick={discard}>{t('system.form.discard')}</Button>
  </div>
{/snippet}

<Panel
  id="net-proxy"
  title={t('system.network.proxy.title')}
  description={t('system.network.proxy.description')}
  footer={d && session.isAdmin ? footer : undefined}
>
  {#if form.loadError && !d}
    <Notice tone="fail" title={t('system.network.proxy.loadError')}>{errorText(form.loadError)}</Notice>
  {:else if !d}
    <Skeleton height="320px" />
  {:else}
    <form id="proxy-{formId}" onsubmit={save} novalidate>
      <fieldset class="stack" disabled={!session.isAdmin}>
        {#if general}<Notice tone="fail">{general}</Notice>{/if}
        <Field id="net-field-proxy-url" label={t('system.network.proxy.url')} optional help={t('system.network.proxy.urlHelp')} error={form.error('proxy.url')}>
          <Input bind:value={d.proxy.url} mono maxlength={255} autocomplete="off" placeholder="http://192.168.1.2:3128" inputmode="url" />
        </Field>
        <div class="grid">
          <Field id="net-field-proxy-username" label={t('system.network.proxy.username')} optional error={form.error('proxy.username')}>
            <Input bind:value={d.proxy.username} maxlength={255} autocomplete="off" />
          </Field>
          <div class="stack-sm">
            <Field
              id="net-field-proxy-password"
              label={t('system.network.proxy.password')}
              optional
              error={form.error('proxy.password')}
              help={rebound && !password && !removePassword ? t('system.network.proxy.passwordAgain') : t('system.network.proxy.passwordHelp')}
            >
              <Input
                type="password"
                bind:value={password}
                maxlength={255}
                autocomplete="new-password"
                disabled={removePassword}
                placeholder={stored && !removePassword ? t('system.notifications.form.secretKeep') : undefined}
              />
            </Field>
            {#if stored}
              <Checkbox bind:checked={removePassword} label={t('system.network.proxy.removePassword')} />
            {/if}
          </div>
        </div>

        <section class="stack-sm" aria-labelledby="proxy-for-{formId}">
          <h3 id="proxy-for-{formId}">{t('system.network.proxy.useFor')}</h3>
          <!-- One literal id per switch: the settings search jumps to them. -->
          <Toggle id="net-field-proxyFor-lists" bind:checked={d.proxyFor.lists} label={t('system.network.proxy.for.lists')} description={t('system.network.proxy.for.listsHelp')} />
          <Toggle id="net-field-proxyFor-updateCheck" bind:checked={d.proxyFor.updateCheck} label={t('system.network.proxy.for.updateCheck')} description={t('system.network.proxy.for.updateCheckHelp')} />
          <Toggle id="net-field-proxyFor-notifications" bind:checked={d.proxyFor.notifications} label={t('system.network.proxy.for.notifications')} description={t('system.network.proxy.for.notificationsHelp')} />
          {#if form.error('proxyFor')}<p class="err">{form.error('proxyFor')}</p>{/if}
        </section>

        {#if d.proxy.url.trim() && anyUse}
          <Notice tone="warn" title={t('system.network.proxy.seesTitle')}>
            <p>{t('system.network.proxy.seesText')}</p>
            {#if d.proxy.username || stored || password}<p>{t('system.network.proxy.cleartext')}</p>{/if}
          </Notice>
        {/if}
        <p class="small muted">{t('system.network.proxy.never')}</p>
      </fieldset>
    </form>
  {/if}
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  h3 {
    font-size: var(--fs-md);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 200px), 1fr));
    gap: var(--sp-4);
    align-items: start;
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
