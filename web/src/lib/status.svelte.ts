// App-wide live status, polled once for the top bar, the pair strip and the
// overview (pages read it instead of polling the same endpoints again):
//   appStatus.overview.data  – GET /system/overview every 10 s
//   appStatus.strip.data     – GET /stats/summary?range=15m every 10 s
//   appStatus.update.data    – GET /system/update once per page load, then
//                              hourly (the server checks GitHub daily); drives
//                              the "update available" dot in the navigation
// Call appStatus.overview.refresh() after changing blocking, LanCache or storage.
// The updates page puts its fresher results into appStatus.update (set()).

import { api, Resource } from './api'

const HOUR = 3_600_000

export const appStatus = {
  overview: new Resource((signal) => api.system.overview({ signal }), { interval: 10_000 }),
  strip: new Resource((signal) => api.stats.summary('15m', { signal }), { interval: 10_000 }),
  update: new Resource((signal) => api.system.update({ signal }), { interval: HOUR }),
}

/** Starts polling (the app shell does this); returns the stop function. */
export function startAppStatus(): () => void {
  appStatus.overview.start()
  appStatus.strip.start()
  appStatus.update.start()
  return () => {
    appStatus.overview.stop()
    appStatus.strip.stop()
    appStatus.update.stop()
  }
}
