// Following an update run of the root helper (docs/ARCHITECTURE.md 14.4).
// POST /system/update/apply only queues a request; the helper then downloads,
// verifies, installs and restarts PiCache and writes its progress, which
// GET /system/update reports as `status`. This polls that endpoint every 2 s.
// While PiCache restarts the requests fail: polling goes on with a growing
// pause for up to 3 minutes before it gives up (phase 'timeout').

import { api, toApiError, type UpdateInfo, type UpdateRun } from '$lib/api'

export type FollowPhase =
  | 'idle'
  /** Accepted, but the helper has not reported this run yet. */
  | 'queued'
  | 'running'
  /** PiCache does not answer (it restarts). */
  | 'restarting'
  | 'succeeded'
  | 'failed'
  | 'rolled-back'
  /** No answer (or no start of the helper) for 3 minutes. */
  | 'timeout'

export interface FollowTarget {
  /** Version being installed. */
  version: string
  /** Version before the update. */
  from: string
  /**
   * `startedAt` of the run known before this one was requested: a run with
   * the same start time is the previous one, not ours.
   */
  previousStartedAt?: string
}

const POLL_MS = 2_000
const MAX_DELAY_MS = 10_000
const PROBE_TIMEOUT_MS = 8_000
const GIVE_UP_MS = 180_000
/** After this long a run that still reports "running" is polled less often. */
const SLOW_AFTER_MS = 600_000

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms)
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(timer)
        resolve()
      },
      { once: true },
    )
  })
}

/** Follows one update run; one per page. */
export class UpdateFollower {
  phase = $state<FollowPhase>('idle')
  /** Version being installed. */
  target = $state('')
  /** Version before the update. */
  from = $state('')
  /** Last progress report of this run. */
  run = $state.raw<UpdateRun | undefined>(undefined)
  /** Version PiCache reported last (after a success: the new one). */
  current = $state('')
  /** Seconds without an answer while restarting. */
  downFor = $state(0)
  /** Seconds since the request was queued, while the helper has not started. */
  queuedFor = $state(0)

  #ctrl: AbortController | null = null
  #last: FollowTarget | undefined
  #onInfo: (info: UpdateInfo) => void

  /** `onInfo` receives every answer (the page shares it with the navigation). */
  constructor(onInfo: (info: UpdateInfo) => void) {
    this.#onInfo = onInfo
  }

  /** Waiting for the run (queued, running or restarting). */
  get active(): boolean {
    return this.phase === 'queued' || this.phase === 'running' || this.phase === 'restarting'
  }

  /** Polls until the run ends, fails or PiCache stays away for 3 minutes. */
  async follow(target: FollowTarget): Promise<void> {
    this.stop()
    const ctrl = new AbortController()
    this.#ctrl = ctrl
    const again = this.#last === target // resume(): keep the progress seen so far
    this.#last = target
    this.target = target.version
    this.from = target.from
    if (!again) this.run = undefined
    this.downFor = 0
    this.queuedFor = 0
    this.phase = this.run ? 'restarting' : 'queued'

    const start = Date.now()
    let downSince: number | undefined
    let delay = POLL_MS
    while (!ctrl.signal.aborted) {
      await sleep(delay, ctrl.signal)
      if (ctrl.signal.aborted) return
      let info: UpdateInfo
      try {
        info = await api.system.update({ signal: AbortSignal.any([ctrl.signal, AbortSignal.timeout(PROBE_TIMEOUT_MS)]) })
      } catch (err) {
        if (ctrl.signal.aborted) return
        if (toApiError(err).code === 'unauthorized') return this.#end('idle') // the app shows the sign-in screen
        // The old process is gone and the new one is not listening yet (or a
        // proxy in front answers 502/503 meanwhile).
        downSince ??= Date.now()
        this.downFor = Math.round((Date.now() - downSince) / 1000)
        this.phase = 'restarting'
        if (Date.now() - downSince > GIVE_UP_MS) return this.#end('timeout')
        delay = Math.min(MAX_DELAY_MS, Math.round(delay * 1.5))
        continue
      }
      downSince = undefined
      delay = Date.now() - start > SLOW_AFTER_MS ? MAX_DELAY_MS : POLL_MS
      this.downFor = 0
      this.current = info.current.version
      this.#onInfo(info)

      const run = info.status
      const ours = !!run && run.version === target.version && run.startedAt !== target.previousStartedAt
      if (!ours) {
        // The helper has not picked up the request yet.
        this.queuedFor = Math.round((Date.now() - start) / 1000)
        this.phase = 'queued'
        if (Date.now() - start > GIVE_UP_MS) return this.#end('timeout')
        continue
      }
      this.run = run
      if (run.state === 'running') this.phase = 'running'
      else return this.#end(run.state)
    }
  }

  /** Starts waiting again after a timeout. */
  resume(): void {
    if (this.#last) void this.follow(this.#last)
  }

  /** Stops polling (page left). */
  stop(): void {
    this.#ctrl?.abort()
    this.#ctrl = null
    if (this.active) this.phase = 'idle'
  }

  #end(phase: FollowPhase): void {
    this.#ctrl = null
    this.phase = phase
  }
}
