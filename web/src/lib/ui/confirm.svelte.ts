// Imperative confirmation dialogs, rendered by <ConfirmHost /> in the app root:
//
//   if (await confirm({ title: t('dns.lists.deleteTitle', { name }), confirmLabel: t('common.action.delete') })) …
//
// With `action`, the dialog runs it (spinner, errors shown in the dialog) and
// resolves true only after it succeeded.

export interface ConfirmOptions {
  title: string
  message?: string
  confirmLabel: string
  cancelLabel?: string
  /** Red confirm button (default true). */
  danger?: boolean
  /** Runs on confirm; the dialog stays open and shows the error if it throws. */
  action?: () => unknown
}

interface Pending extends ConfirmOptions {
  resolve: (ok: boolean) => void
}

let pending = $state<Pending | null>(null)

/** Shows a confirmation dialog; resolves true when confirmed (and `action` succeeded). */
export function confirm(opts: ConfirmOptions): Promise<boolean> {
  pending?.resolve(false)
  return new Promise((resolve) => {
    pending = { ...opts, resolve }
  })
}

/** The dialog currently requested (for ConfirmHost). */
export function pendingConfirm(): Pending | null {
  return pending
}

/** Finishes the current request (for ConfirmHost). */
export function settleConfirm(ok: boolean): void {
  const p = pending
  pending = null
  p?.resolve(ok)
}
