<!--
  @component
  The available release: its notes and how to install it in this
  installation: the "Install update" button (root helper), the Docker
  commands, the command for the host or, for a Debian package, the steps
  (download the .deb with SHA256SUMS and its signature, verify, apt install).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { UpdateInfo, UpdateRelease } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, CopyButton, Icon, Panel, Trans } from '$lib/ui'
  import { REPO } from '../health/about'
  import { safeHref } from './markdown'
  import ReleaseNotes from './ReleaseNotes.svelte'

  interface Props {
    info: UpdateInfo
    release: UpdateRelease
    /** An update is being installed. */
    busy: boolean
    oninstall: () => void
  }

  let { info, release, busy, oninstall }: Props = $props()

  const command = $derived(
    (info.mode === 'docker' ? (info.commands?.docker ?? info.commands?.cli) : info.commands?.cli) || 'sudo picache update',
  )
  const notes = $derived(release.notes?.trim() ?? '')

  // Mode "package": the .deb of this architecture with the checksums and
  // their signature from the same release, verified before apt installs it.
  const pkg = $derived(info.mode === 'package' ? info.package : undefined)
  const pkgUrl = $derived(pkg?.url ? safeHref(pkg.url) : undefined)
  const assetBase = $derived(pkgUrl ? pkgUrl.slice(0, pkgUrl.lastIndexOf('/') + 1) : undefined)
  const aptCommand = $derived(pkg?.file ? `sudo apt install ./${pkg.file}` : '')
  const DEB_GUIDE = `${REPO}/blob/main/docs/DEPLOYMENT.md#debian-package`
</script>

{#snippet external(url: string, text: string)}
  <a href={url} target="_blank" rel="noopener noreferrer" class="ext">
    {text}<Icon name="external" size={14} /><span class="visually-hidden">{` (${t('system.updates.notes.newTab')})`}</span>
  </a>
{/snippet}

<Panel title={t('system.updates.release.title', { version: release.version })}>
  {#snippet actions()}
    {#if release.prerelease}<Badge tone="warn">{t('system.updates.release.prerelease')}</Badge>{/if}
  {/snippet}

  <div class="stack">
    {#if notes}
      <ReleaseNotes source={notes} label={t('system.updates.notes.label', { version: release.version })} />
    {:else}
      <p class="small muted">{t('system.updates.notes.none')}</p>
    {/if}
  </div>

  {#snippet footer()}
    {#if info.mode === 'package'}
      <div class="how stack-sm">
        <p class="small">{t('system.updates.how.package')}</p>
        <ol class="small steps">
          <li>
            <Trans key="system.updates.how.packageDownload">
              {#snippet file()}{#if pkgUrl && pkg?.file}{@render external(pkgUrl, pkg.file)}{:else}<span class="mono">{pkg?.file ?? '–'}</span>{/if}{/snippet}
              {#snippet sums()}{#if assetBase}{@render external(assetBase + 'SHA256SUMS', 'SHA256SUMS')}{:else}<span class="mono">SHA256SUMS</span>{/if}{/snippet}
              {#snippet sig()}{#if assetBase}{@render external(assetBase + 'SHA256SUMS.sig', 'SHA256SUMS.sig')}{:else}<span class="mono">SHA256SUMS.sig</span>{/if}{/snippet}
            </Trans>
          </li>
          <li>
            <Trans key="system.updates.how.packageVerify">
              {#snippet guide()}{@render external(DEB_GUIDE, t('system.updates.how.packageGuide'))}{/snippet}
            </Trans>
          </li>
          {#if aptCommand}
            <li>
              <div class="stack-sm">
                <span>{t('system.updates.how.packageInstall')}</span>
                <div class="cmd">
                  <code class="mono">{aptCommand}</code>
                  <CopyButton text={aptCommand} label={t('system.updates.how.copy')} />
                </div>
              </div>
            </li>
          {/if}
        </ol>
        <p class="small muted">{t('system.updates.how.packageHint')}</p>
      </div>
    {:else if info.mode === 'helper'}
      <p class="small muted grow">{t('system.updates.how.helper', { current: info.current.version })}</p>
      {#if session.isAdmin}
        <Button variant="primary" icon="update" disabled={busy} onclick={oninstall}>
          {t('system.updates.install.button', { version: release.version })}
        </Button>
      {:else}
        <p class="small muted">{t('system.updates.how.adminOnly')}</p>
      {/if}
    {:else}
      <div class="how stack-sm">
        <p class="small">{info.mode === 'docker' ? t('system.updates.how.docker') : t('system.updates.how.manual')}</p>
        <div class="cmd">
          <code class="mono">{command}</code>
          <CopyButton text={command} label={t('system.updates.how.copy')} />
        </div>
        {#if info.mode === 'docker'}
          <p class="small muted">{t('system.updates.how.dockerHint')}</p>
        {:else}
          <p class="small muted">{t('system.updates.how.manualHint')}</p>
        {/if}
      </div>
    {/if}
  {/snippet}
</Panel>

<style>
  .grow {
    flex: 1 1 260px;
  }
  .how {
    flex: 1 1 100%;
    min-width: 0;
  }
  .cmd {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding-left: var(--sp-5);
  }
  .ext {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
    overflow-wrap: anywhere;
  }
  .cmd code {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
    user-select: all;
  }
</style>
