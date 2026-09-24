// Per-browser conveniences in localStorage. Storage can be unavailable
// (private mode, blocked site data), so every access is guarded and the UI
// works without it. Never store secrets or server state here.

const PREFIX = 'picache.'

/** Reads a stored string ('' key prefix is added automatically). */
export function loadPref(key: string): string | null {
  try {
    return window.localStorage.getItem(PREFIX + key)
  } catch {
    return null
  }
}

/** Stores a string; null removes the key. Failures are ignored. */
export function savePref(key: string, value: string | null): void {
  try {
    if (value === null) window.localStorage.removeItem(PREFIX + key)
    else window.localStorage.setItem(PREFIX + key, value)
  } catch {
    /* storage unavailable: the preference lasts for this page load only */
  }
}
