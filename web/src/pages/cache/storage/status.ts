// What a storage target can do next, derived from its mount-guard status
// (internal/storage: probe and checkMarker).

import type { StorageCapabilities, StorageStatus, StorageTargetWithStatus, StoreState } from '$lib/api'
import type { Tone } from '$lib/ui'

export type TargetState =
  | 'active' // online and used by the cache
  | 'ready' // online, can be activated
  | 'checking' // not checked yet
  | 'queued' // host-apply mount requested, waiting for the root helper
  | 'mountFailed' // the root helper reported an error
  | 'notSetUp' // reachable and writable, but no store yet: initialise
  | 'existing' // a store marker was found that this target does not use yet: adopt
  | 'offline'

/** The location was found and passed the write test (older servers: only then is latencyMs set). */
function writable(st: StorageStatus): boolean {
  return st.writable ?? st.latencyMs > 0
}

/**
 * No store is set up for the target yet (status.initialised; older servers do
 * not report it). A target records its store id once it was initialised or
 * adopted, so one with a store id is never "not set up".
 */
function uninitialised(t: StorageTargetWithStatus): boolean {
  return !t.storeId && t.status.initialised !== true
}

export function targetState(t: StorageTargetWithStatus): TargetState {
  const st = t.status
  if (st.online) return t.active ? 'active' : 'ready'
  if (st.applyState === 'queued') return 'queued'
  if (st.applyState?.startsWith('failed')) return 'mountFailed'
  if (!st.checkedAt) return 'checking'
  if (st.storeId && st.storeId !== t.storeId) return 'existing'
  // A target whose store marker is missing is offline (wrong share mounted?), not "not set up".
  if (!st.storeId && uninitialised(t) && writable(st)) return 'notSetUp'
  return 'offline'
}

export const STATE_TONES: Record<TargetState, Tone> = {
  active: 'ok',
  ready: 'ok',
  checking: 'neutral',
  queued: 'info',
  mountFailed: 'fail',
  notSetUp: 'info',
  existing: 'info',
  offline: 'fail',
}

/** Actions offered for a target (the server re-checks everything). */
export function targetActions(t: StorageTargetWithStatus, caps: StorageCapabilities | undefined) {
  const st = t.status
  return {
    /**
     * No store marker at a location that was found (file system known or the
     * write test passed): create a new, empty store there.
     */
    init: !st.online && !st.storeId && (writable(st) || !!st.fsType),
    /** A marker of another (or no recorded) store was found: use it. */
    adopt: !!st.storeId && st.storeId !== t.storeId,
    activate: !t.active && !!t.storeId && st.online,
    apply: t.mode === 'host-apply' && !!caps?.hostApply,
    remove: t.id !== 'local' && !t.active,
  }
}

// The effective minimum free space is min(cache.minFreeBytes, 10 % of the
// disk), but at least 2 GiB on a disk shared with PiCache's own data
// (docs/ARCHITECTURE.md, eviction).
const SHARED_MIN_FREE = 2 * 2 ** 30

/**
 * On a disk shared with PiCache's data (the caller checks sameFsAsData): the
 * minimum free space is the 2 GiB floor.
 */
export function minFreeRaised(store: StoreState): boolean {
  return store.minFreeBytes === SHARED_MIN_FREE
}
