<!--
  @component
  Side panel with a form: used for adding and editing records, forwarders,
  rules, lists, clients and groups. The footer holds Delete (edit mode, left),
  optional extra actions, Cancel and the submit button. Read-only principals
  see the form disabled with a notice.

  <FormPanel bind:open title="Edit rule" submitLabel="Save changes" {saving} {error}
             onsubmit={save} ondelete={remove}>
    …fields…
  </FormPanel>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Notice, SidePanel } from '$lib/ui'

  interface Props {
    open?: boolean
    title: string
    subtitle?: string
    size?: 'md' | 'lg'
    submitLabel: string
    saving?: boolean
    /** General error (errors that name a field are shown next to the field). */
    error?: string
    onsubmit: () => void | Promise<void>
    /** Shows a Delete button (edit mode). */
    ondelete?: () => void
    deleteLabel?: string
    /** Disables the submit button (e.g. nothing changed). */
    submitDisabled?: boolean
    /** Content above the form (details, statistics). */
    header?: Snippet
    /** Extra footer buttons (left of Cancel). */
    extra?: Snippet
    children: Snippet
  }

  let {
    open = $bindable(false),
    title,
    subtitle,
    size = 'md',
    submitLabel,
    saving = false,
    error,
    onsubmit,
    ondelete,
    deleteLabel,
    submitDisabled = false,
    header,
    extra,
    children,
  }: Props = $props()

  const auto = $props.id()
  const formId = `form-${auto}`
  const readOnly = $derived(!session.isAdmin)

  function submit(e: SubmitEvent) {
    e.preventDefault()
    if (readOnly || saving) return
    void onsubmit()
  }
</script>

<SidePanel bind:open {title} {subtitle} {size} dismissible={!saving}>
  <div class="content">
    {@render header?.()}
    {#if !session.canOperate}<Notice>{t('common.state.readOnly')}</Notice>{/if}
    {#if error}<Notice tone="fail">{error}</Notice>{/if}
    <form id={formId} onsubmit={submit} novalidate>
      <fieldset disabled={readOnly} class="stack">
        {@render children()}
      </fieldset>
    </form>
  </div>

  {#snippet actions()}
    {#if ondelete}
      <span class="left">
        <Button variant="danger" icon="trash" disabled={readOnly || saving} onclick={ondelete}>
          {deleteLabel ?? t('common.action.delete')}
        </Button>
      </span>
    {/if}
    {@render extra?.()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form={formId} variant="primary" loading={saving} disabled={readOnly || submitDisabled}>
      {submitLabel}
    </Button>
  {/snippet}
</SidePanel>

<style>
  .content {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
  }
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .left {
    margin-right: auto;
  }
</style>
