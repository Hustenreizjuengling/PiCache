<!--
  @component
  How to allow the DHCP server for an installation (it needs privileges the
  service does not have by default): the installer with --with-dhcp, or
  PICACHE_DHCP with host networking in Docker (NET_RAW only for IPv6
  announcements). Shown while the DHCP server is not available.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { CopyButton, Icon } from '$lib/ui'
  import { REPO } from '../../system/health/about'

  const install = `curl -fsSL ${REPO}/releases/latest/download/get-picache.sh | sudo sh -s -- --with-dhcp`
  const compose = ['network_mode: host', 'environment:', '  PICACHE_DHCP: "on"', 'cap_add: [NET_BIND_SERVICE, SETUID, SETGID, NET_RAW]'].join(
    '\n',
  )
</script>

<div class="howto">
  <div class="stack-sm">
    <h3>{t('dns.dhcp.allow.title')}</h3>
    <p class="small muted">{t('dns.dhcp.allow.text')}</p>
  </div>
  <div class="ways">
    <section class="way">
      <h4>{t('dns.dhcp.allow.installer')}</h4>
      <p class="small">{t('dns.dhcp.allow.installerText')}</p>
      <div class="code">
        <pre class="mono">{install}</pre>
        <span class="copy"><CopyButton text={install} /></span>
      </div>
    </section>
    <section class="way">
      <h4>{t('dns.dhcp.allow.docker')}</h4>
      <p class="small">{t('dns.dhcp.allow.dockerText')}</p>
      <div class="code">
        <pre class="mono">{compose}</pre>
        <span class="copy"><CopyButton text={compose} /></span>
      </div>
    </section>
  </div>
  <p class="small">
    <a href="{REPO}/blob/main/docs/DEPLOYMENT.md#dhcp-server" target="_blank" rel="noopener noreferrer">
      {t('dns.dhcp.allow.docs')}<Icon name="external" size={14} />
    </a>
  </p>
</div>

<style>
  .howto {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    min-width: 0;
    padding: var(--sp-3) var(--sp-4);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  .ways {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  .way {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
  }
  .code {
    position: relative;
    min-width: 0;
  }
  pre {
    margin: 0;
    padding: var(--sp-2) var(--sp-7) var(--sp-2) var(--sp-3);
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    font-size: var(--fs-sm);
    line-height: 1.5;
    white-space: pre;
    overflow-wrap: normal;
  }
  .copy {
    position: absolute;
    top: 4px;
    right: 4px;
  }
  a :global(.icon) {
    margin-left: 4px;
    vertical-align: -2px;
  }
  @media (max-width: 900px) {
    .ways {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  @media (max-width: 480px) {
    .howto {
      padding: var(--sp-3);
    }
  }
</style>
