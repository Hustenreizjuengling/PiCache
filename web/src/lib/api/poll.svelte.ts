// Polling and loading helpers. Polling pauses while the tab is hidden and
// refreshes immediately when it becomes visible again; requests never overlap
// and are aborted when the owner is destroyed.

import { toApiError, type ApiError } from './client'

export type Loader<T> = (signal: AbortSignal) => Promise<T>

export interface ResourceOptions {
  /**
   * Poll interval in ms (0/undefined = load once). A function is asked again
   * before every wait, so the pace can follow the data (e.g. every second
   * while a background job runs, rarely otherwise).
   */
  interval?: number | (() => number)
  /** Stop polling while the tab is hidden (default true). */
  pauseWhenHidden?: boolean
}

/**
 * Reactive loading state for one API call: `data`, `error`, `loading` and
 * `loaded` are $state. Use resource() inside components; use the class
 * directly for app-wide stores and call start()/stop() yourself.
 */
export class Resource<T> {
  /**
   * Last successful result (kept while reloading and after errors). Stored
   * without deep proxies for speed: replace it (set()), never mutate it.
   */
  data = $state.raw<T | undefined>(undefined)
  /** Error of the last attempt (cleared by the next success). */
  error = $state.raw<ApiError | undefined>(undefined)
  /** A request is running. */
  loading = $state(false)
  /** At least one request succeeded. */
  loaded = $state(false)

  #load: Loader<T>
  #opts: ResourceOptions
  #ctrl: AbortController | null = null
  #timer: ReturnType<typeof setTimeout> | undefined
  #running = false
  #lastRun = 0
  #onVisibility = () => this.#visibilityChanged()

  constructor(load: Loader<T>, opts: ResourceOptions = {}) {
    this.#load = load
    this.#opts = opts
  }

  /**
   * Starts loading (and polling). Reactive state read synchronously by the
   * loader before its first await is tracked when start() runs inside an
   * $effect, so resource() reloads when those values change.
   */
  start(): void {
    this.#running = true
    if (this.#pauseHidden()) document.addEventListener('visibilitychange', this.#onVisibility)
    void this.#run()
  }

  /** Stops polling and aborts a running request. */
  stop(): void {
    this.#running = false
    document.removeEventListener('visibilitychange', this.#onVisibility)
    clearTimeout(this.#timer)
    this.#timer = undefined
    this.#ctrl?.abort()
    this.#ctrl = null
  }

  /** Loads now (aborting a running request) and restarts the poll timer. */
  refresh(): Promise<void> {
    return this.#run()
  }

  /** Replaces the data locally (e.g. with the response of a mutation). */
  set(value: T): void {
    this.data = value
    this.loaded = true
    this.error = undefined
  }

  /** The current poll interval in ms (0 = none). */
  #interval(): number {
    const iv = this.#opts.interval
    return (typeof iv === 'function' ? iv() : iv) || 0
  }

  #pauseHidden(): boolean {
    return (this.#opts.pauseWhenHidden ?? true) && !!this.#opts.interval
  }

  #visibilityChanged(): void {
    if (!this.#running) return
    if (document.visibilityState === 'hidden') {
      clearTimeout(this.#timer)
      this.#timer = undefined
    } else if (Date.now() - this.#lastRun >= this.#interval()) {
      void this.#run()
    } else {
      this.#schedule()
    }
  }

  #schedule(): void {
    clearTimeout(this.#timer)
    this.#timer = undefined
    const iv = this.#interval()
    if (!iv || !this.#running) return
    if (this.#pauseHidden() && document.visibilityState === 'hidden') return
    this.#timer = setTimeout(() => void this.#run(), iv)
  }

  async #run(): Promise<void> {
    clearTimeout(this.#timer)
    this.#timer = undefined
    this.#ctrl?.abort()
    const ctrl = new AbortController()
    this.#ctrl = ctrl
    this.#lastRun = Date.now()
    let p: Promise<T>
    try {
      p = this.#load(ctrl.signal) // synchronous part may read reactive state (tracked)
    } catch (err) {
      p = Promise.reject(err)
    }
    this.loading = true
    try {
      const v = await p
      if (ctrl.signal.aborted) return
      this.data = v
      this.error = undefined
      this.loaded = true
    } catch (err) {
      if (ctrl.signal.aborted) return
      const e = toApiError(err)
      if (e.code !== 'aborted') this.error = e
    } finally {
      if (this.#ctrl === ctrl) {
        this.loading = false
        this.#ctrl = null
        this.#schedule()
      }
    }
  }
}

/**
 * Creates a Resource bound to the calling component: it starts when the
 * component mounts, restarts when reactive values read synchronously in
 * `load` change, and stops on destroy. Call during component initialisation.
 *
 *   const lists = resource((signal) => api.filter.lists.list({ signal }))
 *   const top = resource((signal) => api.stats.top('blocked', range, 10, { signal }), { interval: 30_000 })
 */
export function resource<T>(load: Loader<T>, opts: ResourceOptions = {}): Resource<T> {
  const r = new Resource(load, opts)
  $effect(() => {
    r.start()
    return () => r.stop()
  })
  return r
}

/**
 * Low-level polling without reactive state: runs fn every intervalMs (not
 * overlapping, paused while hidden) until the returned stop() is called.
 */
export function poll(fn: (signal: AbortSignal) => Promise<unknown>, intervalMs: number): () => void {
  const r = new Resource(async (signal) => {
    await fn(signal)
  }, { interval: intervalMs })
  r.start()
  return () => r.stop()
}
