import '@fontsource-variable/atkinson-hyperlegible-next'
import '@fontsource-variable/atkinson-hyperlegible-mono'
import './app.css'

import { mount } from 'svelte'
import App from './App.svelte'
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

export default mount(App, { target: document.getElementById('app')! })
