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

  /**
   * Admin rights (an admin session or admin token), whatever the host's
   * configuration lock says: runs the actions that store no configuration
   * (pauses, refreshes, tests, restart) and reads what only admins may read
   * (audit log, notification channels, backups).
   */
  get canOperate(): boolean {
    return this.status?.scope === 'admin'
  }

  /**
   * May change the configuration: admin rights while the host does not lock
   * it (PICACHE_CONFIG_LOCKED). Read-only principals, and admins while the
   * configuration is locked, see disabled actions.
   */
  get isAdmin(): boolean {
    return this.canOperate && !this.configLocked
  }

  /** The host refuses configuration changes from interactive sessions (PICACHE_CONFIG_LOCKED). */
  get configLocked(): boolean {
    return !!this.status?.configLocked
  }

  /** The host allows destructive actions (PICACHE_DESTRUCTIVE_API); false while signed out. */
  get destructiveApi(): boolean {
    return !!this.status?.destructiveApi
  }

  /** May run destructive actions (restore, resets, purges, deleting stores, users or certificates); they are hidden otherwise. */
  get canDestroy(): boolean {
    return this.isAdmin && this.destructiveApi
  }

  /** The signed-in account has the viewer role (reads everything, changes only its own account and read tokens). */
  get isViewer(): boolean {
    return this.user?.role === 'viewer'
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

  /**
   * Reloads /auth/status while the app is shown, after a 403: the role may
   * have changed or the host may lock the configuration now, and the pages
   * should show that. Only a changed status is applied (pages that load
   * depending on the rights would otherwise load again); failures keep it.
   */
  async refresh(): Promise<void> {
    if (this.phase !== 'ready' || this.#refreshing) return
    this.#refreshing = true
    try {
      const st = await api.auth.status()
      if (st.authenticated && JSON.stringify(st) !== JSON.stringify(this.status)) this.status = st
    } catch {
      /* keep what we have */
    } finally {
      this.#refreshing = false
    }
  }

  #refreshing = false

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
