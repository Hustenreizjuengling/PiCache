// Setup hints from /auth/status are English sentences that may end in a
// shell command ("On the PiCache host run: sudo picache setup-token"). The
// command is split off so it can be shown in monospace with a copy button.

export interface SetupHint {
  text: string
  command?: string
}

const COMMAND = /^(?:sudo|docker|podman|picache|journalctl|pct)\s/

/** Splits a hint into its prose and a trailing command, if there is one. */
export function splitHint(hint: string): SetupHint {
  const h = hint.trim()
  const colon = h.lastIndexOf(': ')
  if (colon > 0) {
    const cmd = h.slice(colon + 2).trim()
    if (COMMAND.test(cmd)) return { text: h.slice(0, colon + 1), command: cmd }
  }
  const eg = /^(.*?)\s*\(e\.g\.\s+(.+)\)\.?$/.exec(h)
  if (eg && COMMAND.test(eg[2])) return { text: eg[1] + ':', command: eg[2] }
  return { text: h }
}
