// Long ranges (more than 7 days) read the daily top tables: the overview's
// panels then load one after another instead of all at once, so a Raspberry
// Pi answers them without piling up queries. A request whose range changed
// meanwhile (its signal aborted) is skipped when its turn comes.

import { isLong, type Range } from '$lib/range'

/** Runs requests one after another, skipping aborted ones. */
export class Lane {
  #tail: Promise<unknown> = Promise.resolve()

  run<T>(signal: AbortSignal, fn: () => Promise<T>): Promise<T> {
    const p = this.#tail.then(() => {
      signal.throwIfAborted()
      return fn()
    })
    this.#tail = p.catch(() => undefined)
    return p
  }

  /** Runs `fn` in the lane for a long range, at once otherwise. */
  forRange<T>(range: Range, signal: AbortSignal, fn: () => Promise<T>): Promise<T> {
    return isLong(range) ? this.run(signal, fn) : fn()
  }
}

/** Poll interval of the overview's panels: rarely for long ranges. */
export function pollInterval(range: Range): number {
  return isLong(range) ? 5 * 60_000 : 60_000
}
