// Server-Sent Events with reconnect/backoff, batching and pause. The server
// ends every stream after 1 h and allows at most 16 query and cache streams
// (and 4 application-log streams), so streams reconnect with exponential
// backoff and pause while the tab is hidden.

import { apiUrl, notifyUnauthorized, request, type Query } from './client'
import type { AuthStatus, CacheEvent, LogLevelFilter, LogRecord, QueryEvent, QueryStatus } from './types'

export type StreamState = 'connecting' | 'open' | 'paused' | 'retrying' | 'closed'

export interface StreamOptions<T> {
  query?: Query
  /** Receives events in arrival order, batched (at most every `batchMs`). */
  onEvents: (batch: T[]) => void
  /** Called after a reconnect: events may have been missed, refetch if needed. */
  onReconnect?: () => void
  /** Pause while the tab is hidden (default true). */
  pauseWhenHidden?: boolean
  /** Batch window in ms (default 250). */
  batchMs?: number
  /** Start paused (call resume()). */
  paused?: boolean
}

const MAX_BATCH = 1000 // events kept per batch window; older ones are dropped
const MAX_DELAY = 30_000

/**
 * A live SSE subscription. `state` and `paused` are reactive. Create it in a
 * component and call close() on destroy (or use it inside $effect and return
 * () => stream.close()).
 */
export class LiveStream<T> {
  state = $state<StreamState>('connecting')
  /** Paused by the user (not by tab visibility). */
  paused = $state(false)

  #path: string
  #event: string
  #opts: StreamOptions<T>
  #es: EventSource | null = null
  #buffer: T[] = []
  #flushTimer: ReturnType<typeof setTimeout> | undefined
  #retryTimer: ReturnType<typeof setTimeout> | undefined
  #attempts = 0
  #connectedOnce = false
  #closed = false
  #onVisibility = () => this.#sync()

  constructor(path: string, event: string, opts: StreamOptions<T>) {
    this.#path = path
    this.#event = event
    this.#opts = opts
    this.paused = opts.paused ?? false
    if (opts.pauseWhenHidden ?? true) document.addEventListener('visibilitychange', this.#onVisibility)
    this.#sync()
  }

  /** Stops receiving events until resume(). */
  pause(): void {
    this.paused = true
    this.#sync()
  }

  resume(): void {
    this.paused = false
    this.#sync()
  }

  /** Ends the subscription for good. */
  close(): void {
    this.#closed = true
    document.removeEventListener('visibilitychange', this.#onVisibility)
    this.#disconnect()
    this.#flush()
    clearTimeout(this.#flushTimer)
    this.state = 'closed'
  }

  #hidden(): boolean {
    return (this.#opts.pauseWhenHidden ?? true) && document.visibilityState === 'hidden'
  }

  #sync(): void {
    if (this.#closed) return
    if (this.paused || this.#hidden()) {
      this.#disconnect()
      this.state = 'paused'
      return
    }
    if (!this.#es && this.#retryTimer === undefined) this.#connect()
  }

  #connect(): void {
    this.state = this.#attempts > 0 ? 'retrying' : 'connecting'
    const es = new EventSource(apiUrl(this.#path, this.#opts.query))
    this.#es = es
    es.onopen = () => {
      this.#attempts = 0
      this.state = 'open'
      if (this.#connectedOnce) this.#opts.onReconnect?.()
      this.#connectedOnce = true
    }
    es.addEventListener(this.#event, (e) => {
      let v: T
      try {
        v = JSON.parse((e as MessageEvent<string>).data) as T
      } catch {
        return
      }
      this.#buffer.push(v)
      if (this.#buffer.length > MAX_BATCH) this.#buffer.splice(0, this.#buffer.length - MAX_BATCH)
      this.#flushTimer ??= setTimeout(() => this.#flush(), this.#opts.batchMs ?? 250)
    })
    es.onerror = () => {
      // Closed by the server (1 h limit), network loss, 401 or 429: back off
      // ourselves instead of the browser's fixed retry.
      this.#disconnect()
      this.#scheduleRetry()
    }
  }

  #disconnect(): void {
    clearTimeout(this.#retryTimer)
    this.#retryTimer = undefined
    if (this.#es) {
      this.#es.onerror = null
      this.#es.close()
      this.#es = null
    }
  }

  #scheduleRetry(): void {
    if (this.#closed) return
    this.#attempts++
    this.state = 'retrying'
    const base = Math.min(MAX_DELAY, 1000 * 2 ** Math.min(this.#attempts - 1, 5))
    const delay = base * (0.8 + Math.random() * 0.4)
    this.#retryTimer = setTimeout(async () => {
      this.#retryTimer = undefined
      if (this.#attempts >= 2 && !(await this.#sessionValid())) {
        this.close()
        notifyUnauthorized()
        return
      }
      this.#sync()
    }, delay)
  }

  async #sessionValid(): Promise<boolean> {
    try {
      const st = await request<AuthStatus>('GET', '/auth/status', { allowUnauthorized: true })
      return st.authenticated
    } catch {
      return true // server unreachable: keep retrying
    }
  }

  #flush(): void {
    this.#flushTimer = undefined
    if (this.#buffer.length === 0) return
    const batch = this.#buffer
    this.#buffer = []
    this.#opts.onEvents(batch)
  }
}

/** Live query log (`event: query`), filtered server-side by client and statuses. */
export function streamQueries(
  filter: { client?: string; status?: QueryStatus[] },
  opts: Omit<StreamOptions<QueryEvent>, 'query'>,
): LiveStream<QueryEvent> {
  return new LiveStream<QueryEvent>('/stream/queries', 'query', { ...opts, query: { ...filter } })
}

/** Live cache requests (`event: request`). */
export function streamCache(opts: Omit<StreamOptions<CacheEvent>, 'query'>): LiveStream<CacheEvent> {
  return new LiveStream<CacheEvent>('/stream/cache', 'request', opts)
}

/** Live application log (`event: record`; admins, at most 4 streams), filtered server-side. */
export function streamSystemLog(
  filter: { level?: LogLevelFilter; component?: string },
  opts: Omit<StreamOptions<LogRecord>, 'query'>,
): LiveStream<LogRecord> {
  return new LiveStream<LogRecord>('/stream/system-log', 'record', { ...opts, query: { ...filter } })
}
