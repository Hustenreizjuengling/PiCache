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
})

export default mount(App, { target: document.getElementById('app')! })
