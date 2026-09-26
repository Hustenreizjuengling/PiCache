<!--
  @component
  The query types a rule applies to: toggle chips for the common types, a
  field for any other type (a name such as NAPTR, or TYPE65), and "every
  type except these". Empty means every type. A or AAAA also covers HTTPS
  and SVCB queries (their address hints would give the addresses away).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { Button, Checkbox, Icon, Input } from '$lib/ui'

  interface Props {
    value: string[]
    negate: boolean
    /** Validation message of the server (qtypes, qtypes[i], qtypesNegate). */
    error?: string
  }

  let { value = $bindable(), negate = $bindable(), error }: Props = $props()

  const COMMON = ['A', 'AAAA', 'HTTPS', 'CNAME', 'MX', 'TXT', 'SRV', 'PTR', 'ANY']
  const MAX = 16

  const auto = $props.id()
  let other = $state('')
  let otherError = $state<string | undefined>(undefined)

  const extra = $derived(value.filter((ty) => !COMMON.includes(ty)))
  const covers = $derived(!negate && (value.includes('A') || value.includes('AAAA')))

  function set(next: string[]) {
    value = next
    if (next.length === 0) negate = false
  }

  function toggle(ty: string) {
    set(value.includes(ty) ? value.filter((x) => x !== ty) : [...value, ty])
  }

  function add() {
    const ty = other.trim().toUpperCase()
    otherError = undefined
    if (!ty) return
    if (!/^(TYPE\d{1,5}|[A-Z][A-Z0-9-]{0,15})$/.test(ty)) {
      otherError = t('dns.rules.qtypes.invalid')
      return
    }
    if (!value.includes(ty)) set([...value, ty])
    other = ''
  }

  function onKey(e: KeyboardEvent) {
    if (e.key !== 'Enter') return
    e.preventDefault()
    add()
  }
</script>

<fieldset class="qt" aria-describedby="qt-{auto}-help">
  <legend>{t('dns.rules.qtypes')}</legend>
  <div class="chips">
    {#each COMMON as ty (ty)}
      <button type="button" class="chip mono" aria-pressed={value.includes(ty)} onclick={() => toggle(ty)}>
        {#if value.includes(ty)}<Icon name="check" size={14} />{/if}{ty}
      </button>
    {/each}
    {#each extra as ty (ty)}
      <span class="chip mono on">
        {ty}
        <button type="button" class="x" aria-label={t('dns.rules.qtypes.remove', { type: ty })} onclick={() => toggle(ty)}>
          <Icon name="close" size={14} />
        </button>
      </span>
    {/each}
  </div>
  <div class="other">
    <Input
      bind:value={other}
      size="sm"
      mono
      aria-label={t('dns.rules.qtypes.other')}
      placeholder={t('dns.rules.qtypes.otherPlaceholder')}
      maxlength={16}
      invalid={!!otherError}
      onkeydown={onKey}
    />
    <Button size="sm" icon="plus" disabled={!other.trim() || value.length >= MAX} onclick={add}>{t('dns.rules.qtypes.add')}</Button>
  </div>
  {#if otherError}<p class="err">{otherError}</p>{/if}
  <Checkbox
    checked={negate}
    disabled={value.length === 0}
    label={t('dns.rules.qtypes.negate')}
    onchange={(on) => (negate = on)}
  />
  {#if error}<p class="err"><Icon name="alert" size={16} />{error}</p>{/if}
  <p class="help" id="qt-{auto}-help">
    {value.length === 0 ? t('dns.rules.qtypes.helpEmpty') : negate ? t('dns.rules.qtypes.helpNegate') : t('dns.rules.qtypes.helpSome')}
    {#if covers}{t('dns.rules.qtypes.covers')}{/if}
  </p>
</fieldset>

<style>
  .qt {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 6px;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-2);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 26px;
    padding: 0 var(--sp-2);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-pill);
    background: var(--surface);
    color: var(--text-2);
    font-size: var(--fs-sm);
    cursor: pointer;
  }
  .chip:hover {
    color: var(--text);
  }
  .chip:focus-visible,
  .x:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
  .chip[aria-pressed='true'],
  .chip.on {
    border-color: var(--text);
    background: var(--text);
    color: var(--surface);
  }
  .chip.on {
    padding-right: 2px;
    cursor: default;
  }
  .x {
    display: inline-flex;
    padding: 2px;
    border: 0;
    border-radius: var(--r-pill);
    background: none;
    color: inherit;
    cursor: pointer;
  }
  .chip:disabled,
  .x:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .other {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
  }
  .other :global(.wrap) {
    flex: 0 1 180px;
  }
  .help {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .err {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
