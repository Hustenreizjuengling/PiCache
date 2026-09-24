// Restarting PiCache from the UI. POST /system/restart makes the process
// exit (code 75) and systemd or Docker start it again. The new process is
// recognised by its start time, or by the session being gone (a restore
// revokes all sessions); until then /auth/status is polled.

import { api } from '$lib/api'

export type RestartPhase = 'idle' | 'waiting' | 'back' | 'timeout'

const POLL_MS = 1_500
const PROBE_TIMEOUT_MS = 4_000
const GIVE_UP_MS = 180_000
/** Without a known start time, a probe that never failed counts as restarted after this. */
const BLIND_GRACE_MS = 20_000

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

/** Result of waiting: whether the signed-in session survived the restart. */
export interface RestartOutcome {
  signedIn: boolean
}

/** Requests a restart and follows it until PiCache answers again. One per component. */
export class RestartWatcher {
  phase = $state<RestartPhase>('idle')
  /** Seconds since the restart was requested. */
  elapsed = $state(0)

  #before: string | undefined
  #ctrl: AbortController | null = null

  /** Asks the server to restart; throws the API error (e.g. 403) if it refuses. */
  async request(): Promise<void> {
    try {
      this.#before = (await api.system.info()).startedAt
    } catch {
      this.#before = undefined
    }
    await api.system.restart()
  }

  /**
   * Polls until the new process answers. Resolves with the outcome, or null
   * when it was cancelled or gave up (phase 'timeout').
   */
  async wait(): Promise<RestartOutcome | null> {
    this.stop()
    const ctrl = new AbortController()
    this.#ctrl = ctrl
    this.phase = 'waiting'
    const start = Date.now()
    let sawDown = false
    while (!ctrl.signal.aborted) {
      await sleep(POLL_MS, ctrl.signal)
      if (ctrl.signal.aborted) return null
      const waited = Date.now() - start
      this.elapsed = Math.round(waited / 1000)
      if (waited > GIVE_UP_MS) {
        this.phase = 'timeout'
        return null
      }
      const signal = AbortSignal.any([ctrl.signal, AbortSignal.timeout(PROBE_TIMEOUT_MS)])
      try {
        const st = await api.auth.status({ signal })
        if (!st.authenticated) return this.#done(false)
        const started = (await api.system.info({ signal })).startedAt
        if (this.#before !== undefined ? started !== this.#before : sawDown || waited > BLIND_GRACE_MS) {
          return this.#done(true)
        }
      } catch {
        sawDown = true // the old process is gone, the new one is not listening yet
      }
    }
    return null
  }

  /** Stops polling (component destroyed or the user closed the dialog). */
  stop(): void {
    this.#ctrl?.abort()
    this.#ctrl = null
    if (this.phase === 'waiting') this.phase = 'idle'
  }

  #done(signedIn: boolean): RestartOutcome {
    this.#ctrl = null
    this.phase = 'back'
    return { signedIn }
  }
}
