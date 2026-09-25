// A small Markdown reader for release notes (the CHANGELOG.md section of a
// GitHub release). The notes come from the network, so they are never turned
// into HTML: this produces a tree that ReleaseNotes.svelte renders as Svelte
// elements with text content only. Supported: ATX headings, paragraphs,
// bullet and ordered lists (nested, with wrapped continuation lines), fenced
// code, block quotes, thematic breaks, inline code, bold, italics, links
// (inline, reference and <autolinks>, bare https URLs) and backslash escapes.
// Everything else (HTML, tables, images) stays text. Only https links become
// links.

export type Inline =
  | { type: 'text'; text: string }
  | { type: 'code'; text: string }
  | { type: 'strong'; children: Inline[] }
  | { type: 'em'; children: Inline[] }
  | { type: 'link'; href: string; children: Inline[] }

export type Block =
  | { type: 'heading'; level: number; content: Inline[] }
  | { type: 'paragraph'; content: Inline[] }
  | { type: 'list'; ordered: boolean; start: number; items: Block[][] }
  | { type: 'code'; text: string }
  | { type: 'quote'; blocks: Block[] }
  | { type: 'rule' }

/** Link reference definitions (`[label]: url`), keyed by the lower-cased label. */
type Refs = Map<string, string>

const MAX_DEPTH = 8

/** The URL if it is an absolute https URL, else undefined (such links render as text). */
export function safeHref(url: string): string | undefined {
  try {
    const u = new URL(url.trim())
    return u.protocol === 'https:' && u.hostname ? u.href : undefined
  } catch {
    return undefined
  }
}

// ---------------------------------------------------------------- blocks

const FENCE = /^ {0,3}(`{3,}|~{3,})(.*)$/
const HEADING = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/
const RULE = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/
const QUOTE = /^ {0,3}>[ ]?(.*)$/
const ITEM = /^( {0,3})([-*+]|\d{1,9}[.)])(?:([ \t]+)(.*))?$/
const REF_DEF = /^ {0,3}\[([^\]]{1,200})\]:[ \t]*<?([^\s>]+)>?(?:[ \t]+(?:"[^"]*"|'[^']*'|\([^)]*\)))?[ \t]*$/

function isBlank(line: string): boolean {
  return line.trim() === ''
}

function indentOf(line: string): number {
  return line.length - line.trimStart().length
}

function isItem(line: string): RegExpMatchArray | null {
  const m = ITEM.exec(line)
  if (!m) return null
  if (RULE.test(line)) return null // "- - -" and "* * *" are rules
  return m
}

/**
 * A line that ends a paragraph because a new block starts. As in CommonMark,
 * an ordered list interrupts a paragraph only when it starts at 1, so a
 * wrapped sentence ending in "… version
2. …" stays text.
 */
function startsBlock(line: string): boolean {
  if (FENCE.test(line) || HEADING.test(line) || RULE.test(line) || QUOTE.test(line)) return true
  const item = isItem(line)
  return !!item && item[4] !== undefined && item[4].trim() !== '' && (!/\d/.test(item[2]) || parseInt(item[2], 10) === 1)
}

function parseBlocks(lines: string[], refs: Refs, depth: number): Block[] {
  const out: Block[] = []
  let i = 0
  while (i < lines.length) {
    const line = lines[i]
    if (isBlank(line)) {
      i++
      continue
    }

    const fence = FENCE.exec(line)
    if (fence && !(fence[1][0] === '`' && fence[2].includes('`'))) {
      const marker = fence[1]
      const indent = indentOf(line)
      const body: string[] = []
      i++
      while (i < lines.length) {
        const l = lines[i]
        const close = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(l)
        if (close && close[1][0] === marker[0] && close[1].length >= marker.length) {
          i++
          break
        }
        body.push(l.slice(Math.min(indent, indentOf(l))))
        i++
      }
      out.push({ type: 'code', text: body.join('\n') })
      continue
    }

    const heading = HEADING.exec(line)
    if (heading) {
      out.push({ type: 'heading', level: heading[1].length, content: parseInline(heading[2] ?? '', refs) })
      i++
      continue
    }

    if (RULE.test(line)) {
      out.push({ type: 'rule' })
      i++
      continue
    }

    if (QUOTE.test(line)) {
      const body: string[] = []
      while (i < lines.length && !isBlank(lines[i])) {
        const q = QUOTE.exec(lines[i])
        if (q) body.push(q[1])
        else if (startsBlock(lines[i])) break
        else body.push(lines[i]) // lazy continuation of the quoted paragraph
        i++
      }
      out.push({ type: 'quote', blocks: depth < MAX_DEPTH ? parseBlocks(body, refs, depth + 1) : paragraphOf(body, refs) })
      continue
    }

    const first = isItem(line)
    if (first) {
      const ordered = /\d/.test(first[2])
      const items: Block[][] = []
      while (i < lines.length) {
        const m = isItem(lines[i])
        if (!m || /\d/.test(m[2]) !== ordered) break
        const gap = m[3] ?? ''
        // Content starts after the marker and its spaces (one space when the
        // text is indented code or the item is empty).
        const width = m[1].length + m[2].length + (gap.length >= 1 && gap.length <= 4 && m[4] ? gap.length : 1)
        const body: string[] = [m[4] ?? '']
        i++
        while (i < lines.length) {
          const l = lines[i]
          if (isBlank(l)) {
            let j = i + 1
            while (j < lines.length && isBlank(lines[j])) j++
            if (j < lines.length && indentOf(lines[j]) >= width) {
              for (; i < j; i++) body.push('')
              continue
            }
            break
          }
          if (indentOf(l) >= width) {
            body.push(l.slice(width))
            i++
            continue
          }
          // A wrapped line with less indentation continues the item's text.
          if (!isBlank(body[body.length - 1]) && !startsBlock(l) && !isItem(l)) {
            body.push(l.trim())
            i++
            continue
          }
          break
        }
        items.push(depth < MAX_DEPTH ? parseBlocks(body, refs, depth + 1) : paragraphOf(body, refs))
        let j = i
        while (j < lines.length && isBlank(lines[j])) j++
        const next = j < lines.length ? isItem(lines[j]) : null
        if (next && /\d/.test(next[2]) === ordered && indentOf(lines[j]) <= 3) i = j
        else break
      }
      out.push({ type: 'list', ordered, start: ordered ? parseInt(first[2], 10) : 1, items })
      continue
    }

    const para: string[] = [line]
    i++
    while (i < lines.length && !isBlank(lines[i]) && !startsBlock(lines[i])) {
      para.push(lines[i])
      i++
    }
    out.push(...paragraphOf(para, refs))
  }
  return out
}

function paragraphOf(lines: string[], refs: Refs): Block[] {
  const text = lines.map((l) => l.trim()).join('\n').trim()
  return text ? [{ type: 'paragraph', content: parseInline(text, refs) }] : []
}

// ---------------------------------------------------------------- inline

const PUNCT = /[!-/:-@[-`{-~]/
const BARE_URL = /^https:\/\/[^\s<>"]+/i
const TRAILING = /[.,:;!?'"*_~]+$/

function pushText(out: Inline[], text: string): void {
  if (!text) return
  const last = out[out.length - 1]
  if (last?.type === 'text') last.text += text
  else out.push({ type: 'text', text })
}

/** Index of the `]` closing the `[` at `open`, skipping code spans and escapes; -1 if none. */
function closingBracket(s: string, open: number): number {
  let depth = 0
  for (let i = open; i < s.length; i++) {
    const c = s[i]
    if (c === '\\') i++
    else if (c === '`') {
      const run = /^`+/.exec(s.slice(i))![0]
      const end = s.indexOf(run, i + run.length)
      if (end > 0) i = end + run.length - 1
    } else if (c === '[') depth++
    else if (c === ']' && --depth === 0) return i
  }
  return -1
}

/** Parses `(url "title")` at `pos`; returns the URL and the index after `)`. */
function inlineDestination(s: string, pos: number): { url: string; end: number } | null {
  if (s[pos] !== '(') return null
  let i = pos + 1
  while (s[i] === ' ' || s[i] === '\n') i++
  let url = ''
  if (s[i] === '<') {
    const close = s.indexOf('>', i)
    if (close < 0) return null
    url = s.slice(i + 1, close)
    i = close + 1
  } else {
    let depth = 0
    const start = i
    for (; i < s.length; i++) {
      const c = s[i]
      if (c === '\\') {
        i++
        continue
      }
      if (c === ' ' || c === '\n') break
      if (c === '(') depth++
      else if (c === ')') {
        if (depth === 0) break
        depth--
      }
    }
    url = s.slice(start, i)
  }
  while (s[i] === ' ' || s[i] === '\n') i++
  if (s[i] === '"' || s[i] === "'" || s[i] === '(') {
    const closeChar = s[i] === '(' ? ')' : s[i]
    const close = s.indexOf(closeChar, i + 1)
    if (close < 0) return null
    i = close + 1
    while (s[i] === ' ' || s[i] === '\n') i++
  }
  if (s[i] !== ')') return null
  return { url: url.replace(/\\([!-/:-@[-`{-~])/g, '$1'), end: i + 1 }
}

function isWordChar(c: string | undefined): boolean {
  return !!c && /[\p{L}\p{N}]/u.test(c)
}

/** Finds the closing emphasis delimiter for an opener at `from`; -1 if none. */
function closingDelimiter(s: string, delim: string, from: number): number {
  let i = from
  while (i < s.length) {
    const c = s[i]
    if (c === '\\') {
      i += 2
      continue
    }
    if (c === '`') {
      const run = /^`+/.exec(s.slice(i))![0]
      const end = s.indexOf(run, i + run.length)
      i = end > 0 ? end + run.length : i + run.length
      continue
    }
    if (s.startsWith(delim, i)) {
      const before = s[i - 1]
      const after = s[i + delim.length]
      const flanking = before !== undefined && !/\s/.test(before)
      const intraword = delim[0] === '_' && isWordChar(after)
      // `**` inside `*…*` must not close the single delimiter.
      const longer = delim.length === 1 && s[i + 1] === delim
      if (flanking && !intraword && !longer && i > from) return i
      if (longer) {
        i += 2
        continue
      }
    }
    i++
  }
  return -1
}

export function parseInline(s: string, refs: Refs = new Map(), depth = 0): Inline[] {
  const out: Inline[] = []
  let i = 0
  let text = ''
  const flush = () => {
    pushText(out, text)
    text = ''
  }
  while (i < s.length) {
    const c = s[i]

    if (c === '\\' && i + 1 < s.length && PUNCT.test(s[i + 1])) {
      text += s[i + 1]
      i += 2
      continue
    }

    if (c === '\n') {
      text += ' '
      i++
      continue
    }

    if (c === '`') {
      const run = /^`+/.exec(s.slice(i))![0]
      const end = s.indexOf(run, i + run.length)
      // The closing run must have exactly the same length.
      if (end > 0 && s[end + run.length] !== '`') {
        let code = s.slice(i + run.length, end).replace(/\n/g, ' ')
        if (code.length > 2 && code.startsWith(' ') && code.endsWith(' ') && code.trim()) code = code.slice(1, -1)
        flush()
        out.push({ type: 'code', text: code })
        i = end + run.length
      } else {
        text += run
        i += run.length
      }
      continue
    }

    if ((c === '*' || c === '_') && depth < MAX_DEPTH) {
      const double = s[i + 1] === c
      const delim = double ? c + c : c
      const next = s[i + delim.length]
      const canOpen = next !== undefined && !/\s/.test(next) && !(c === '_' && isWordChar(s[i - 1]))
      if (canOpen) {
        const end = closingDelimiter(s, delim, i + delim.length)
        if (end > 0) {
          flush()
          const children = parseInline(s.slice(i + delim.length, end), refs, depth + 1)
          out.push(double ? { type: 'strong', children } : { type: 'em', children })
          i = end + delim.length
          continue
        }
      }
      text += delim
      i += delim.length
      continue
    }

    const image = c === '!' && s[i + 1] === '['
    if ((c === '[' || image) && depth < MAX_DEPTH) {
      const open = image ? i + 1 : i
      const close = closingBracket(s, open)
      if (close > 0) {
        const label = s.slice(open + 1, close)
        let url: string | undefined
        let end = close + 1
        const dest = inlineDestination(s, close + 1)
        if (dest) {
          url = dest.url
          end = dest.end
        } else if (s[close + 1] === '[') {
          const refClose = s.indexOf(']', close + 2)
          if (refClose > 0) {
            const key = s.slice(close + 2, refClose).trim() || label
            url = refs.get(normalizeLabel(key))
            if (url !== undefined) end = refClose + 1
          }
        } else {
          url = refs.get(normalizeLabel(label))
        }
        if (url !== undefined) {
          flush()
          const children = parseInline(label, refs, depth + 1)
          const href = safeHref(url)
          if (href) out.push({ type: 'link', href, children })
          else for (const n of children) if (n.type === 'text') pushText(out, n.text)
            else out.push(n)
          i = end
          continue
        }
      }
      text += image ? '![' : '['
      i += image ? 2 : 1
      continue
    }

    if (c === '<') {
      const m = /^<(https?:\/\/[^\s<>]+)>/i.exec(s.slice(i))
      if (m) {
        const href = safeHref(m[1])
        flush()
        if (href) out.push({ type: 'link', href, children: [{ type: 'text', text: m[1] }] })
        else pushText(out, m[1])
        i += m[0].length
        continue
      }
    }

    if ((c === 'h' || c === 'H') && !isWordChar(s[i - 1])) {
      const m = BARE_URL.exec(s.slice(i))
      if (m) {
        let url = m[0].replace(TRAILING, '')
        // A closing parenthesis belongs to the URL only when it opened one.
        while (url.endsWith(')') && (url.match(/\(/g)?.length ?? 0) < (url.match(/\)/g)?.length ?? 0)) {
          url = url.slice(0, -1).replace(TRAILING, '')
        }
        const href = safeHref(url)
        if (href) {
          flush()
          out.push({ type: 'link', href, children: [{ type: 'text', text: url }] })
          i += url.length
          continue
        }
      }
    }

    text += c
    i++
  }
  flush()
  return out
}

function normalizeLabel(label: string): string {
  return label.trim().replace(/\s+/g, ' ').toLowerCase()
}

// ---------------------------------------------------------------- entry points

/**
 * Removes HTML comments (template comments are not meant to be read),
 * repeatedly, so that nested markers cannot leave a new comment behind.
 */
function stripComments(s: string): string {
  let prev: string
  do {
    prev = s
    s = s.replace(/<!--[\s\S]*?-->/g, '')
  } while (s !== prev)
  return s
}

/** Parses release notes into blocks. */
export function parseMarkdown(source: string): Block[] {
  const refs: Refs = new Map()
  const lines: string[] = []
  const text = stripComments(source.replace(/\r\n?/g, '\n')).replace(/\t/g, '    ')
  let inFence = false
  for (const line of text.split('\n')) {
    if (FENCE.test(line)) inFence = !inFence
    const def = inFence ? null : REF_DEF.exec(line)
    if (def) {
      const key = normalizeLabel(def[1])
      if (!refs.has(key)) refs.set(key, def[2])
      continue
    }
    lines.push(line)
  }
  return parseBlocks(lines, refs, 0)
}

function inlineLength(nodes: Inline[]): number {
  let n = 0
  for (const x of nodes) n += x.type === 'text' || x.type === 'code' ? x.text.length : inlineLength(x.children)
  return n
}

function blockLength(b: Block): number {
  switch (b.type) {
    case 'heading':
    case 'paragraph':
      return inlineLength(b.content)
    case 'code':
      return b.text.length
    case 'quote':
      return b.blocks.reduce((n, x) => n + blockLength(x), 0)
    case 'list':
      return b.items.reduce((n, item) => n + item.reduce((m, x) => m + blockLength(x), 0), 0)
    case 'rule':
      return 0
  }
}

/**
 * The first blocks of the notes, about `budget` characters of text (list
 * items are kept whole, at least one block is shown). `truncated` says
 * whether anything was left out.
 */
export function limitBlocks(blocks: Block[], budget: number): { blocks: Block[]; truncated: boolean } {
  const out: Block[] = []
  let used = 0
  for (let i = 0; i < blocks.length; i++) {
    const b = blocks[i]
    const len = blockLength(b)
    if (used + len <= budget || out.length === 0) {
      if (b.type === 'list' && used + len > budget) {
        // A long first list: keep the items that fit (at least one).
        const items: Block[][] = []
        for (const item of b.items) {
          const n = item.reduce((m, x) => m + blockLength(x), 0)
          if (items.length > 0 && used + n > budget) break
          items.push(item)
          used += n
        }
        out.push({ ...b, items })
        return { blocks: out, truncated: items.length < b.items.length || i < blocks.length - 1 }
      }
      out.push(b)
      used += len
      continue
    }
    if (b.type === 'list') {
      const items: Block[][] = []
      for (const item of b.items) {
        const n = item.reduce((m, x) => m + blockLength(x), 0)
        if (used + n > budget) break
        items.push(item)
        used += n
      }
      if (items.length > 0) out.push({ ...b, items })
    }
    // A heading with nothing after it reads oddly: drop it.
    while (out.length > 1 && out[out.length - 1].type === 'heading') out.pop()
    return { blocks: out, truncated: true }
  }
  return { blocks: out, truncated: false }
}
