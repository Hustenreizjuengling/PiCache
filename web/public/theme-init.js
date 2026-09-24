// Applies the manual theme override (Settings: system / light / dark) before
// the app loads, so a dark-mode user does not see a light flash.
;(function () {
  try {
    var t = window.localStorage.getItem('picache.theme')
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t)
  } catch (e) {
    /* storage unavailable (private mode, blocked): follow the system */
  }
})()
