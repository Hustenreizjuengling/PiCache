// App-wide live status, polled once for the top bar, the pair strip and the
// overview (pages read it instead of polling the same endpoints again):
//   appStatus.overview.data  – GET /system/overview every 10 s
//   appStatus.strip.data     – GET /stats/summary?range=15m every 10 s
// Call appStatus.overview.refresh() after changing blocking, LanCache or storage.

import { api, Resource } from './api'

export const appStatus = {
  overview: new Resource((signal) => api.system.overview({ signal }), { interval: 10_000 }),
  strip: new Resource((signal) => api.stats.summary('15m', { signal }), { interval: 10_000 }),
}

/** Starts polling (the app shell does this); returns the stop function. */
export function startAppStatus(): () => void {
  appStatus.overview.start()
  appStatus.strip.start()
  return () => {
    appStatus.overview.stop()
    appStatus.strip.stop()
  }
}
