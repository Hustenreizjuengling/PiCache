import '@fontsource-variable/atkinson-hyperlegible-next'
import '@fontsource-variable/atkinson-hyperlegible-mono'
import './app.css'

import { mount } from 'svelte'
import App from './App.svelte'
import { initI18n } from './i18n/index.svelte'
import { setApiHooks } from './lib/api'
import { session } from './lib/session.svelte'

setApiHooks({
  unauthorized: () => session.lost(),
  misdirected: (message) => session.misdirected(message),
  forbidden: () => void session.refresh(),
  synced: () => void session.refresh(),
})

// Another tab or the command line may have changed the rights or the synced
// sections meanwhile (a follower switched on or off, where no request of
// this tab would fail): check again when the app comes back into view, at
// most once a minute.
let statusChecked = Date.now()
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState !== 'visible' || Date.now() - statusChecked < 60_000) return
  statusChecked = Date.now()
  void session.refresh()
})

// The language known at start (this browser's pick, else the browser's) is
// loaded before anything is shown, so no English flashes up first. (No
// top-level await: it would split the entry chunk.)
void initI18n().then(() => mount(App, { target: document.getElementById('app')! }))
