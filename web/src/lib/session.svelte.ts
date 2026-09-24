// Authentication state of the app: which screen to show (setup, login, the
// app) and who is signed in. A 401 from any request brings back the login
// screen without losing the current URL, so the user returns to the same page.

import { applyServerLocale } from '../i18n/index.svelte'
import { api, toApiError, type ApiError, type AuthStatus, type User } from './api'

export type Phase = 'loading' | 'error' | 'misdirected' | 'setup' | 'login' | 'ready'

class Session {
  phase = $state<Phase>('loading')
  status = $state<AuthStatus | undefined>(undefined)
  /** Why /auth/status failed (phase 'error'). */
  error = $state<ApiError | undefined>(undefined)
  /** The session ended while the app was open (the login page says so). */
  expired = $state(false)
  /** Server message for an unknown host name (phase 'misdirected'). */
  misdirectedMessage = $state('')

  /** The signed-in user. */
  get user(): User | undefined {
    return this.status?.user
  }

  /** Admin scope: may change settings. Read-only principals see disabled actions. */
  get isAdmin(): boolean {
    return this.status?.scope === 'admin'
  }

  /** Loads /auth/status and selects the screen. */
  async load(): Promise<void> {
    try {
      const st = await api.auth.status()
      this.status = st
      this.error = undefined
      applyServerLocale(st.language)
      this.phase = st.setupRequired ? 'setup' : st.authenticated ? 'ready' : 'login'
    } catch (err) {
      const e = toApiError(err)
      if (e.code === 'misdirected') {
        this.misdirected(e.message)
        return
      }
      this.error = e
      this.phase = 'error'
    }
  }

  /** After a successful login or setup. */
  async signedIn(): Promise<void> {
    this.expired = false
    await this.load()
  }

  /** A request returned 401 while the app was shown. */
  lost(): void {
    if (this.phase !== 'ready') return
    this.expired = true
    this.phase = 'login'
  }

  /** The server rejected our host name (DNS-rebinding guard). */
  misdirected(message: string): void {
    this.misdirectedMessage = message
    this.phase = 'misdirected'
  }

  async logout(): Promise<void> {
    try {
      await api.auth.logout()
    } catch {
      /* already signed out or unreachable: show the login screen anyway */
    }
    this.expired = false
    await this.load()
  }
}

export const session = new Session()
