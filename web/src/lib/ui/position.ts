// Places popover elements (top layer, position: fixed) next to their anchor.
// Popovers escape overflow clipping, so menus work inside scrolling tables.

const MARGIN = 8

/** Below the anchor (above if there is more room there), aligned to its start or end edge. */
export function placeMenu(anchor: HTMLElement, el: HTMLElement, align: 'start' | 'end'): void {
  const a = anchor.getBoundingClientRect()
  const vw = document.documentElement.clientWidth
  const vh = window.innerHeight
  const below = vh - a.bottom - MARGIN
  const above = a.top - MARGIN
  const s = el.style
  s.position = 'fixed'
  s.margin = '0'
  s.inset = 'auto'
  if (below >= 200 || below >= above) {
    s.top = `${a.bottom + 4}px`
    s.maxHeight = `${Math.max(120, below - 4)}px`
  } else {
    s.bottom = `${vh - a.top + 4}px`
    s.maxHeight = `${Math.max(120, above - 4)}px`
  }
  if (align === 'end') {
    s.right = `${Math.max(MARGIN, vw - a.right)}px`
    s.maxWidth = `${Math.max(160, a.right - MARGIN)}px`
  } else {
    s.left = `${Math.max(MARGIN, a.left)}px`
    s.maxWidth = `${Math.max(160, vw - a.left - MARGIN)}px`
  }
}

/** Centred above the anchor (below when there is no room), kept inside the viewport. */
export function placeTooltip(anchor: HTMLElement, el: HTMLElement): void {
  const a = anchor.getBoundingClientRect()
  const vw = document.documentElement.clientWidth
  const s = el.style
  s.position = 'fixed'
  s.margin = '0'
  s.inset = 'auto'
  s.maxWidth = `${Math.min(320, vw - 2 * MARGIN)}px`
  const r = el.getBoundingClientRect()
  let top = a.top - r.height - 6
  if (top < MARGIN) top = a.bottom + 6
  let left = a.left + a.width / 2 - r.width / 2
  left = Math.max(MARGIN, Math.min(left, vw - r.width - MARGIN))
  s.top = `${top}px`
  s.left = `${left}px`
}
