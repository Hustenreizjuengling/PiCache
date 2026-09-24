// Light/dark theme: follows prefers-color-scheme unless the user picked a
// manual override (stored per browser). public/theme-init.js applies the
// stored override before the app loads.

import { loadPref, savePref } from './storage'

export type ThemeChoice = 'system' | 'light' | 'dark'

const media = window.matchMedia('(prefers-color-scheme: dark)')
const stored = loadPref('theme')

const initial: ThemeChoice = stored === 'light' || stored === 'dark' ? stored : 'system'
let choice = $state<ThemeChoice>(initial)
let systemDark = $state(media.matches)
media.addEventListener('change', (e) => {
  systemDark = e.matches
})

function apply(c: ThemeChoice): void {
  if (c === 'system') document.documentElement.removeAttribute('data-theme')
  else document.documentElement.setAttribute('data-theme', c)
}
apply(initial)

/** Reactive theme state. `effective` is what is shown right now. */
export const theme = {
  get choice(): ThemeChoice {
    return choice
  },
  get effective(): 'light' | 'dark' {
    return choice === 'system' ? (systemDark ? 'dark' : 'light') : choice
  },
  set(c: ThemeChoice): void {
    choice = c
    savePref('theme', c === 'system' ? null : c)
    apply(c)
  },
}

/** Whether the user asked for reduced motion (charts and strips skip animation). */
export function prefersReducedMotion(): boolean {
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches
}
