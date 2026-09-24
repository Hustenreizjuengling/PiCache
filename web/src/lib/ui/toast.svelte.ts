// Toast notifications confirming actions ("Blocklist added") or reporting
// failures. Rendered by <Toasts /> in the app root.

import { errorText } from '../errors'

export type ToastKind = 'success' | 'error' | 'info'

export interface ToastItem {
  id: number
  kind: ToastKind
  message: string
}

const MAX_VISIBLE = 4
const DURATION: Record<ToastKind, number> = { success: 4000, info: 5000, error: 8000 }

let nextId = 1
const items = $state<ToastItem[]>([])
const timers = new Map<number, ReturnType<typeof setTimeout>>()

function push(kind: ToastKind, message: string): number {
  const id = nextId++
  items.push({ id, kind, message })
  while (items.length > MAX_VISIBLE) dismiss(items[0].id)
  timers.set(
    id,
    setTimeout(() => dismiss(id), DURATION[kind]),
  )
  return id
}

/** Removes a toast. */
export function dismiss(id: number): void {
  clearTimeout(timers.get(id))
  timers.delete(id)
  const i = items.findIndex((x) => x.id === id)
  if (i >= 0) items.splice(i, 1)
}

/** Keeps a toast open while the pointer or focus is on it. */
export function hold(id: number): void {
  clearTimeout(timers.get(id))
}

/** Restarts the dismiss timer after hold(). */
export function release(id: number): void {
  const it = items.find((x) => x.id === id)
  if (!it) return
  clearTimeout(timers.get(id))
  timers.set(
    id,
    setTimeout(() => dismiss(id), DURATION[it.kind]),
  )
}

/** Visible toasts (reactive, oldest first). */
export function toasts(): readonly ToastItem[] {
  return items
}

/**
 * toast.success('Blocklist added') · toast.error(err) · toast.info('…').
 * error() accepts an Error/ApiError (translated via errorText) or a string.
 */
export const toast = {
  success: (message: string) => push('success', message),
  info: (message: string) => push('info', message),
  error: (err: unknown) => push('error', typeof err === 'string' ? err : errorText(err)),
}
