<!--
  @component
  Add or edit a storage target. Adding is a two-step wizard: first the kind
  (local folder, SMB, NFS) and who mounts it (you, or PiCache's root helper
  when it is installed), then the connection details. The NAS password is
  write-only: it is never shown and only sent when typed (or cleared).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    type StorageCapabilities,
    type StorageKind,
    type StorageMode,
    type StorageTarget,
    type StorageTargetInput,
    type StorageTargetWithStatus,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, Checkbox, Dialog, Field, Input, Notice, Select, toast } from '$lib/ui'
  import { isIP } from '../shared/util'

  interface Props {
    open?: boolean
    /** The target to edit; undefined adds a new one. */
    target?: StorageTargetWithStatus
    caps: StorageCapabilities | undefined
    onsaved: (target: StorageTarget, created: boolean) => void
  }

  let { open = $bindable(false), target, caps, onsaved }: Props = $props()

  const SMB_VERSIONS = ['3.1.1', '3.0', '3']
  const NFS_VERSIONS = ['4.2', '4.1', '4', '3']
  const REL_PATH_RE = /^[A-Za-z0-9._/-]*$/ // the part of a path below the mount root
  const SHARE_RE = /^[A-Za-z0-9._$-]{1,80}$/
  const EXPORT_RE = /^\/[A-Za-z0-9._/-]{1,255}$/
  const ACCOUNT_RE = /^[A-Za-z0-9._@-]{0,64}$/
  const SEGMENT_RE = /^[A-Za-z0-9._-]{1,64}$/

  const mountRoot = $derived(caps?.mountRoot || '/srv/picache')
  const hostApply = $derived(!!caps?.hostApply)
  const builtin = $derived(target?.id === 'local')

  let step = $state<1 | 2>(1)
  let kind = $state<StorageKind>('smb')
  let mode = $state<StorageMode>('external')
  let name = $state('')
  let path = $state('')
  let server = $state('')
  let share = $state('')
  let exportPath = $state('')
  let subdir = $state('')
  let username = $state('')
  let domain = $state('')
  let password = $state('')
  let clearPassword = $state(false)
  let smbVersion = $state('3.1.1')
  let smbSeal = $state(false)
  let nfsVersion = $state('4.2')
  let nconnect = $state(4)
  let requireMountpoint = $state(true)

  let saving = $state(false)
  let error = $state<unknown>(undefined)
  let attempted = $state(false)

  // Fill the form each time the dialog opens.
  $effect(() => {
    if (!open) return
    untrack(() => {
      const tg = target
      step = tg ? 2 : 1
      kind = tg?.kind ?? 'smb'
      mode = tg?.mode ?? 'external'
      name = tg?.name ?? ''
      path = tg?.path ?? ''
      server = tg?.server ?? ''
      share = tg?.share ?? ''
      exportPath = tg?.export ?? ''
      subdir = tg?.subdir ?? ''
      username = tg?.username ?? ''
      domain = tg?.domain ?? ''
      password = ''
      clearPassword = false
      smbVersion = tg?.smbVersion || SMB_VERSIONS[0]
      smbSeal = tg?.smbSeal ?? false
      nfsVersion = tg?.nfsVersion || NFS_VERSIONS[0]
      nconnect = tg?.nfsNconnect || 4
      requireMountpoint = tg?.requireMountpoint ?? true
      error = undefined
      attempted = false
    })
  })

  function chooseKind(k: StorageKind) {
    kind = k
    if (k === 'local') mode = 'external'
  }

  function next() {
    if (!path.trim()) path = `${mountRoot}/${kind === 'local' ? 'disk' : 'nas'}`
    step = 2
  }

  // ---- client-side checks (the server validates again)

  const netfs = $derived(kind !== 'local')
  const external = $derived(kind === 'local' || mode === 'external')

  function pathProblem(): string | undefined {
    if (!external) return undefined
    const p = path.trim()
    if (!p) return t('common.field.required')
    // Compare with forward slashes so development builds on Windows work too.
    const norm = p.replace(/\\/g, '/')
    const root = mountRoot.replace(/\\/g, '/').replace(/\/+$/, '')
    if (!norm.startsWith(`${root}/`) || norm.length <= root.length + 1) return t('cache.targetForm.pathOutside', { root: mountRoot })
    const rest = norm.slice(root.length)
    if (!REL_PATH_RE.test(rest) || rest.includes('//') || /\/\.\.?(\/|$)/.test(rest)) return t('cache.targetForm.pathInvalid')
    return undefined
  }

  function subdirProblem(): string | undefined {
    const s = subdir.trim().replace(/^\/+|\/+$/g, '')
    if (!s) return undefined
    const segs = s.split('/')
    if (segs.length > 8 || segs.some((x) => x === '.' || x === '..' || !SEGMENT_RE.test(x))) return t('cache.targetForm.subdirInvalid')
    return undefined
  }

  const problems = $derived.by(() => {
    const p: Record<string, string | undefined> = {}
    if (!name.trim()) p.name = t('common.field.required')
    if (builtin) return p
    p.path = pathProblem()
    p.subdir = subdirProblem()
    if (netfs && !isIP(server.trim())) p.server = t('cache.targetForm.serverInvalid')
    if (kind === 'smb') {
      if (!SHARE_RE.test(share.trim()) || share.trim() === '.' || share.trim() === '..') p.share = t('cache.targetForm.shareInvalid')
      if (!ACCOUNT_RE.test(username.trim())) p.username = t('cache.targetForm.accountInvalid')
      if (!ACCOUNT_RE.test(domain.trim())) p.domain = t('cache.targetForm.accountInvalid')
      if (password && !username.trim()) p.password = t('cache.targetForm.passwordNeedsUser')
      if (/[\u0000-\u001f\u007f]/.test(password)) p.password = t('cache.targetForm.passwordInvalid')
    }
    if (kind === 'nfs') {
      if (!EXPORT_RE.test(exportPath.trim()) || exportPath.includes('..')) p.exportPath = t('cache.targetForm.exportInvalid')
      if (!Number.isInteger(nconnect) || nconnect < 1 || nconnect > 16) p.nconnect = t('cache.targetForm.nconnectInvalid')
    }
    return p
  })
  const valid = $derived(Object.values(problems).every((v) => !v))

  /** Client problem (after the first submit) or the server's message for this field. */
  function err(field: string, serverField = field): string | undefined {
    return (attempted ? problems[field] : undefined) ?? fieldError(error, serverField)
  }

  const otherError = $derived.by(() => {
    if (!error) return undefined
    const fields = ['name', 'path', 'server', 'share', 'export', 'subdir', 'username', 'domain', 'password', 'smbVersion', 'nfsVersion', 'nfsNconnect']
    return fields.some((f) => fieldError(error, f)) ? undefined : errorText(error)
  })

  function buildInput(): StorageTargetInput {
    if (builtin && target) {
      return {
        name: name.trim(),
        kind: target.kind,
        mode: target.mode,
        path: target.path,
        server: '',
        share: '',
        export: '',
        subdir: '',
        username: '',
        domain: '',
        smbVersion: '',
        smbSeal: false,
        nfsVersion: '',
        nfsNconnect: 0,
        requireMountpoint: false,
      }
    }
    const input: StorageTargetInput = {
      name: name.trim(),
      kind,
      mode: kind === 'local' ? 'external' : mode,
      path: external ? path.trim() : '',
      server: netfs ? server.trim() : '',
      share: kind === 'smb' ? share.trim() : '',
      export: kind === 'nfs' ? exportPath.trim() : '',
      subdir: subdir.trim().replace(/^\/+|\/+$/g, ''),
      username: kind === 'smb' ? username.trim() : '',
      domain: kind === 'smb' ? domain.trim() : '',
      smbVersion: kind === 'smb' ? smbVersion : '',
      smbSeal: kind === 'smb' && smbSeal,
      nfsVersion: kind === 'nfs' ? nfsVersion : '',
      nfsNconnect: kind === 'nfs' ? nconnect : 0,
      requireMountpoint: kind === 'local' ? requireMountpoint : true,
    }
    // Write-only: undefined keeps the saved password, '' removes it.
    if (kind === 'smb') {
      if (password) input.password = password
      else if (clearPassword) input.password = ''
    }
    return input
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    attempted = true
    if (!valid) return
    saving = true
    error = undefined
    try {
      const input = buildInput()
      const saved = target ? await api.storage.update(target.id, input) : await api.storage.create(input)
      password = ''
      toast.success(target ? t('cache.targetForm.saved', { name: saved.name }) : t('cache.targetForm.added', { name: saved.name }))
      open = false
      onsaved(saved, !target)
    } catch (err) {
      error = err
    } finally {
      saving = false
    }
  }

  const containerHint = $derived(
    caps?.container === 'docker' || caps?.container === 'podman'
      ? t('cache.targetForm.hintDocker', { root: mountRoot })
      : caps?.container === 'lxc'
        ? t('cache.targetForm.hintLxc', { root: mountRoot })
        : undefined,
  )
</script>

{#snippet choice(group: string, value: string, checked: boolean, title: string, text: string, onpick: () => void)}
  <label class={['choice', checked && 'checked']}>
    <input type="radio" name={group} {value} {checked} onchange={onpick} />
    <span class="choice-text">
      <span class="choice-title">{title}</span>
      <span class="choice-desc">{text}</span>
    </span>
  </label>
{/snippet}

<Dialog
  bind:open
  size="md"
  dismissible={!saving}
  title={target ? t('cache.targetForm.editTitle', { name: target.name }) : t('cache.targetForm.addTitle')}
  subtitle={target ? undefined : t('cache.targetForm.step', { step, total: 2 })}
>
  <form id="storage-target" class="stack" onsubmit={submit} novalidate>
    {#if otherError}<Notice tone="fail">{otherError}</Notice>{/if}

    {#if step === 1}
      <fieldset>
        <legend>{t('cache.targetForm.kindQuestion')}</legend>
        {@render choice('kind', 'local', kind === 'local', t('cache.targetForm.kindLocal'), t('cache.targetForm.kindLocalText', { root: mountRoot }), () => chooseKind('local'))}
        {@render choice('kind', 'smb', kind === 'smb', t('cache.targetForm.kindSmb'), t('cache.targetForm.kindSmbText'), () => chooseKind('smb'))}
        {@render choice('kind', 'nfs', kind === 'nfs', t('cache.targetForm.kindNfs'), t('cache.targetForm.kindNfsText'), () => chooseKind('nfs'))}
      </fieldset>

      {#if kind !== 'local'}
        <fieldset>
          <legend>{t('cache.targetForm.modeQuestion')}</legend>
          {@render choice('mode', 'external', mode === 'external', t('cache.targetForm.modeExternal'), t('cache.targetForm.modeExternalText'), () => (mode = 'external'))}
          {#if hostApply}
            {@render choice('mode', 'host-apply', mode === 'host-apply', t('cache.targetForm.modeHostApply'), t('cache.targetForm.modeHostApplyText', { root: mountRoot }), () => (mode = 'host-apply'))}
          {:else}
            <p class="muted small">{t('cache.targetForm.hostApplyUnavailable')}</p>
          {/if}
        </fieldset>
      {/if}
      {#if containerHint}<Notice tone="info">{containerHint}</Notice>{/if}
    {:else}
      {#if builtin}
        <Notice tone="info">{t('cache.targetForm.builtinNote')}</Notice>
      {:else if target}
        <p class="muted small">{t(`cache.target.kind.${kind}`)}</p>
        {#if netfs && (hostApply || mode === 'host-apply')}
          <Field label={t('cache.targetForm.modeQuestion')}>
            <Select
              bind:value={() => mode, (v) => (mode = v === 'host-apply' ? 'host-apply' : 'external')}
              options={[
                { value: 'external', label: t('cache.targetForm.modeExternal') },
                { value: 'host-apply', label: t('cache.targetForm.modeHostApply'), disabled: !hostApply },
              ]}
            />
          </Field>
        {/if}
      {/if}

      <Field label={t('common.label.name')} required error={err('name')}>
        <Input bind:value={name} maxlength={64} placeholder={t('cache.targetForm.namePlaceholder')} />
      </Field>

      {#if !builtin}
        {#if netfs}
          <div class="grid">
            <Field label={t('cache.target.server')} required help={t('cache.targetForm.serverHelp')} error={err('server')}>
              <Input bind:value={server} mono placeholder="192.168.1.20" />
            </Field>
            {#if kind === 'smb'}
              <Field label={t('cache.target.share')} required error={err('share')}>
                <Input bind:value={share} mono placeholder="picache" />
              </Field>
            {:else}
              <Field label={t('cache.target.export')} required error={err('exportPath', 'export')}>
                <Input bind:value={exportPath} mono placeholder="/volume1/picache" />
              </Field>
            {/if}
          </div>
        {/if}

        {#if kind === 'smb'}
          <div class="grid">
            <Field label={t('cache.target.username')} optional help={t('cache.targetForm.usernameHelp')} error={err('username')}>
              <Input bind:value={username} mono autocomplete="off" />
            </Field>
            <Field label={t('cache.target.domain')} optional error={err('domain')}>
              <Input bind:value={domain} mono autocomplete="off" />
            </Field>
          </div>
          <Field
            label={t('cache.target.password')}
            optional
            help={target?.hasPassword ? t('cache.targetForm.passwordKeep') : t('cache.targetForm.passwordHelp')}
            error={err('password')}
          >
            <Input type="password" bind:value={password} autocomplete="new-password" disabled={clearPassword} />
          </Field>
          {#if target?.hasPassword}
            <Checkbox bind:checked={clearPassword} label={t('cache.targetForm.passwordClear')} />
          {/if}
          <div class="grid">
            <Field label={t('cache.target.smbVersion')} error={err('smbVersion')}>
              <Select bind:value={smbVersion} options={SMB_VERSIONS.map((v) => ({ value: v, label: v }))} />
            </Field>
            <div class="check">
              <Checkbox bind:checked={smbSeal} label={t('cache.target.smbSeal')} description={t('cache.targetForm.smbSealHelp')} />
            </div>
          </div>
        {/if}

        {#if kind === 'nfs'}
          <div class="grid">
            <Field label={t('cache.target.nfsVersion')} error={err('nfsVersion')}>
              <Select bind:value={nfsVersion} options={NFS_VERSIONS.map((v) => ({ value: v, label: v }))} />
            </Field>
            <Field label={t('cache.target.nconnect')} help={t('cache.targetForm.nconnectHelp')} error={err('nconnect', 'nfsNconnect')}>
              <Input
                type="number"
                min={1}
                max={16}
                bind:value={() => nconnect, (v) => (nconnect = Number(v ?? 0))}
              />
            </Field>
          </div>
        {/if}

        {#if external}
          <Field
            label={kind === 'local' ? t('cache.targetForm.folder') : t('cache.targetForm.mountPath')}
            required
            help={kind === 'local' ? t('cache.targetForm.folderHelp', { root: mountRoot }) : t('cache.targetForm.mountPathHelp', { root: mountRoot })}
            error={err('path')}
          >
            <Input bind:value={path} mono placeholder={`${mountRoot}/nas`} />
          </Field>
        {:else}
          <p class="muted small">{t('cache.targetForm.hostApplyPath', { root: mountRoot })}</p>
        {/if}

        <Field label={t('cache.target.subdir')} optional help={t('cache.targetForm.subdirHelp')} error={err('subdir')}>
          <Input bind:value={subdir} mono placeholder="cache" />
        </Field>

        {#if kind === 'local'}
          <Checkbox
            bind:checked={requireMountpoint}
            label={t('cache.targetForm.requireMount')}
            description={t('cache.targetForm.requireMountHelp')}
          />
        {/if}
      {/if}
    {/if}
  </form>

  {#snippet actions()}
    {#if step === 1}
      <Button variant="ghost" onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
      <Button variant="primary" onclick={next}>{t('cache.targetForm.next')}</Button>
    {:else}
      {#if !target}
        <Button variant="ghost" disabled={saving} onclick={() => (step = 1)}>{t('common.action.back')}</Button>
      {:else}
        <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
      {/if}
      <Button variant="primary" type="submit" form="storage-target" loading={saving}>
        {target ? t('common.action.save') : t('cache.targetForm.add')}
      </Button>
    {/if}
  {/snippet}
</Dialog>

<style>
  fieldset {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    border: 0;
    min-width: 0;
  }
  legend {
    margin-bottom: var(--sp-2);
    padding: 0;
    font-weight: 600;
  }
  .choice {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    cursor: pointer;
  }
  .choice:hover {
    background: var(--surface-2);
  }
  .choice.checked {
    border-color: var(--text);
    box-shadow: inset 0 0 0 1px var(--text);
  }
  .choice input {
    margin-top: 3px;
    accent-color: var(--text);
  }
  .choice:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .choice-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .choice-title {
    font-weight: 600;
  }
  .choice-desc {
    font-size: var(--fs-sm);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  .check {
    display: flex;
    align-items: flex-end;
    padding-bottom: 6px;
  }
  @media (max-width: 560px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
