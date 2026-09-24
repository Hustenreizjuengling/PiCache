// A small QR code encoder (ISO/IEC 18004) for otpauth:// URIs, so the TOTP
// secret never leaves the browser for rendering. Byte mode, error correction
// level M, versions 1–10 (up to 213 bytes; an otpauth URI is at most ~180).
// The structure follows the well-known reference algorithm: encode the data,
// add Reed–Solomon error correction per block, interleave, place the modules
// and pick the mask with the lowest penalty.

/** A QR symbol: `modules[y][x]` is true for dark modules. */
export interface QrCode {
  version: number
  size: number
  modules: boolean[][]
}

const MAX_VERSION = 10

// Level M, index = version (0 unused).
const ECC_CODEWORDS_PER_BLOCK = [-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26]
const NUM_ECC_BLOCKS = [-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5]
const FORMAT_BITS_M = 0 // error correction level M in the format information

/** Modules available for data and ECC in a version (everything but function patterns). */
function rawDataModules(ver: number): number {
  let n = (16 * ver + 128) * ver + 64
  if (ver >= 2) {
    const align = Math.floor(ver / 7) + 2
    n -= (25 * align - 10) * align - 55
    if (ver >= 7) n -= 36
  }
  return n
}

/** Data codewords (bytes) of a version at level M. */
export function dataCodewords(ver: number): number {
  return Math.floor(rawDataModules(ver) / 8) - ECC_CODEWORDS_PER_BLOCK[ver] * NUM_ECC_BLOCKS[ver]
}

/** Bits of the character count field in byte mode. */
function countBits(ver: number): number {
  return ver <= 9 ? 8 : 16
}

// ---------------------------------------------------------------- Reed–Solomon over GF(2^8), polynomial 0x11D

function gfMul(x: number, y: number): number {
  let z = 0
  for (let i = 7; i >= 0; i--) {
    z = (z << 1) ^ ((z >>> 7) * 0x11d)
    z ^= ((y >>> i) & 1) * x
  }
  return z & 0xff
}

/** Generator polynomial of the given degree (highest coefficient omitted). */
export function rsDivisor(degree: number): number[] {
  const out = new Array<number>(degree).fill(0)
  out[degree - 1] = 1
  let root = 1
  for (let i = 0; i < degree; i++) {
    for (let j = 0; j < out.length; j++) {
      out[j] = gfMul(out[j], root)
      if (j + 1 < out.length) out[j] ^= out[j + 1]
    }
    root = gfMul(root, 0x02)
  }
  return out
}

/** Error correction codewords for one block. */
export function rsRemainder(data: readonly number[], divisor: readonly number[]): number[] {
  const out = divisor.map(() => 0)
  for (const b of data) {
    const factor = b ^ (out.shift() as number)
    out.push(0)
    divisor.forEach((coef, i) => (out[i] ^= gfMul(coef, factor)))
  }
  return out
}

// ---------------------------------------------------------------- data encoding

function appendBits(bits: number[], value: number, len: number): void {
  for (let i = len - 1; i >= 0; i--) bits.push((value >>> i) & 1)
}

/** Mode, count, data, terminator and padding as data codewords. */
function encodeData(bytes: Uint8Array, ver: number): number[] {
  const capacity = dataCodewords(ver) * 8
  const bits: number[] = []
  appendBits(bits, 0b0100, 4) // byte mode
  appendBits(bits, bytes.length, countBits(ver))
  for (const b of bytes) appendBits(bits, b, 8)
  appendBits(bits, 0, Math.min(4, capacity - bits.length))
  appendBits(bits, 0, (8 - (bits.length % 8)) % 8)
  for (let pad = 0xec; bits.length < capacity; pad ^= 0xec ^ 0x11) appendBits(bits, pad, 8)
  const out: number[] = []
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0
    for (let j = 0; j < 8; j++) v = (v << 1) | bits[i + j]
    out.push(v)
  }
  return out
}

/** Splits into blocks, appends ECC to each and interleaves them. */
function addEccAndInterleave(data: number[], ver: number): number[] {
  const numBlocks = NUM_ECC_BLOCKS[ver]
  const eccLen = ECC_CODEWORDS_PER_BLOCK[ver]
  const raw = Math.floor(rawDataModules(ver) / 8)
  const numShort = numBlocks - (raw % numBlocks)
  const shortLen = Math.floor(raw / numBlocks)
  const divisor = rsDivisor(eccLen)
  const blocks: number[][] = []
  for (let i = 0, k = 0; i < numBlocks; i++) {
    const dat = data.slice(k, k + shortLen - eccLen + (i < numShort ? 0 : 1))
    k += dat.length
    const ecc = rsRemainder(dat, divisor)
    if (i < numShort) dat.push(0) // placeholder, skipped when interleaving
    blocks.push(dat.concat(ecc))
  }
  const out: number[] = []
  for (let i = 0; i < blocks[0].length; i++) {
    blocks.forEach((block, j) => {
      if (i !== shortLen - eccLen || j >= numShort) out.push(block[i])
    })
  }
  return out
}

// ---------------------------------------------------------------- module placement

class Matrix {
  readonly version: number
  readonly size: number
  readonly modules: boolean[][]
  readonly isFunction: boolean[][]

  constructor(version: number) {
    this.version = version
    this.size = version * 4 + 17
    this.modules = Array.from({ length: this.size }, () => new Array<boolean>(this.size).fill(false))
    this.isFunction = Array.from({ length: this.size }, () => new Array<boolean>(this.size).fill(false))
  }

  setFunction(x: number, y: number, dark: boolean): void {
    this.modules[y][x] = dark
    this.isFunction[y][x] = true
  }

  drawFunctionPatterns(): void {
    const s = this.size
    for (let i = 0; i < s; i++) {
      this.setFunction(6, i, i % 2 === 0)
      this.setFunction(i, 6, i % 2 === 0)
    }
    this.drawFinder(3, 3)
    this.drawFinder(s - 4, 3)
    this.drawFinder(3, s - 4)
    const pos = alignmentPositions(this.version)
    const n = pos.length
    for (let i = 0; i < n; i++) {
      for (let j = 0; j < n; j++) {
        // Skip the three corners occupied by finder patterns.
        if ((i === 0 && j === 0) || (i === 0 && j === n - 1) || (i === n - 1 && j === 0)) continue
        this.drawAlignment(pos[i], pos[j])
      }
    }
    this.drawFormatBits(0) // reserves the area; redrawn with the chosen mask
    this.drawVersion()
  }

  drawFinder(x: number, y: number): void {
    for (let dy = -4; dy <= 4; dy++) {
      for (let dx = -4; dx <= 4; dx++) {
        const dist = Math.max(Math.abs(dx), Math.abs(dy))
        const xx = x + dx
        const yy = y + dy
        if (xx >= 0 && xx < this.size && yy >= 0 && yy < this.size) this.setFunction(xx, yy, dist !== 2 && dist !== 4)
      }
    }
  }

  drawAlignment(x: number, y: number): void {
    for (let dy = -2; dy <= 2; dy++) {
      for (let dx = -2; dx <= 2; dx++) this.setFunction(x + dx, y + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1)
    }
  }

  drawFormatBits(mask: number): void {
    const bits = formatBits(mask)
    const s = this.size
    const bit = (i: number) => ((bits >>> i) & 1) !== 0
    for (let i = 0; i <= 5; i++) this.setFunction(8, i, bit(i))
    this.setFunction(8, 7, bit(6))
    this.setFunction(8, 8, bit(7))
    this.setFunction(7, 8, bit(8))
    for (let i = 9; i < 15; i++) this.setFunction(14 - i, 8, bit(i))
    for (let i = 0; i < 8; i++) this.setFunction(s - 1 - i, 8, bit(i))
    for (let i = 8; i < 15; i++) this.setFunction(8, s - 15 + i, bit(i))
    this.setFunction(8, s - 8, true) // the dark module
  }

  drawVersion(): void {
    if (this.version < 7) return
    const bits = versionBits(this.version)
    for (let i = 0; i < 18; i++) {
      const dark = ((bits >>> i) & 1) !== 0
      const a = this.size - 11 + (i % 3)
      const b = Math.floor(i / 3)
      this.setFunction(a, b, dark)
      this.setFunction(b, a, dark)
    }
  }

  /** Places the codewords in the zigzag order (two columns, bottom-up then top-down). */
  drawCodewords(data: number[]): void {
    let i = 0
    const total = data.length * 8
    for (let right = this.size - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5 // skip the vertical timing pattern
      for (let vert = 0; vert < this.size; vert++) {
        for (let j = 0; j < 2; j++) {
          const x = right - j
          const upward = ((right + 1) & 2) === 0
          const y = upward ? this.size - 1 - vert : vert
          if (!this.isFunction[y][x] && i < total) {
            this.modules[y][x] = ((data[i >>> 3] >>> (7 - (i & 7))) & 1) !== 0
            i++
          }
        }
      }
    }
  }

  /** XORs a mask pattern onto the data modules (applying it twice undoes it). */
  applyMask(mask: number): void {
    for (let y = 0; y < this.size; y++) {
      for (let x = 0; x < this.size; x++) {
        if (!this.isFunction[y][x] && maskBit(mask, x, y)) this.modules[y][x] = !this.modules[y][x]
      }
    }
  }

  penalty(): number {
    const s = this.size
    const m = this.modules
    let result = 0
    for (let line = 0; line < 2; line++) {
      for (let a = 0; a < s; a++) {
        const at = (b: number) => (line === 0 ? m[a][b] : m[b][a])
        let runColor = false
        let run = 0
        const history = [0, 0, 0, 0, 0, 0, 0]
        for (let b = 0; b < s; b++) {
          if (at(b) === runColor) {
            run++
            if (run === 5) result += 3
            else if (run > 5) result++
          } else {
            this.addHistory(run, history)
            if (!runColor) result += this.finderLike(history) * 40
            runColor = at(b)
            run = 1
          }
        }
        if (runColor) {
          this.addHistory(run, history)
          run = 0
        }
        this.addHistory(run + s, history)
        result += this.finderLike(history) * 40
      }
    }
    for (let y = 0; y < s - 1; y++) {
      for (let x = 0; x < s - 1; x++) {
        const c = m[y][x]
        if (c === m[y][x + 1] && c === m[y + 1][x] && c === m[y + 1][x + 1]) result += 3
      }
    }
    let dark = 0
    for (const row of m) for (const c of row) if (c) dark++
    const total = s * s
    result += (Math.ceil(Math.abs(dark * 20 - total * 10) / total) - 1) * 10
    return result
  }

  private addHistory(run: number, history: number[]): void {
    if (history[0] === 0) run += this.size // light border before the first run
    history.pop()
    history.unshift(run)
  }

  /** 1:1:3:1:1 dark/light patterns with 4 light modules on one side. */
  private finderLike(h: number[]): number {
    const n = h[1]
    const core = n > 0 && h[2] === n && h[3] === n * 3 && h[4] === n && h[5] === n
    return (core && h[0] >= n * 4 && h[6] >= n ? 1 : 0) + (core && h[6] >= n * 4 && h[0] >= n ? 1 : 0)
  }
}

/** Centre coordinates of the alignment patterns (rows and columns). */
function alignmentPositions(ver: number): number[] {
  if (ver === 1) return []
  const n = Math.floor(ver / 7) + 2
  const step = Math.floor((ver * 8 + n * 3 + 5) / (n * 4 - 4)) * 2
  const out = [6]
  for (let pos = ver * 4 + 17 - 7; out.length < n; pos -= step) out.splice(1, 0, pos)
  return out
}

/** 15-bit format information (level M) for a mask, BCH-coded and XOR-masked. */
export function formatBits(mask: number): number {
  const data = (FORMAT_BITS_M << 3) | mask
  let rem = data
  for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537)
  return ((data << 10) | rem) ^ 0x5412
}

/** 18-bit version information (versions 7 and up), BCH-coded. */
export function versionBits(ver: number): number {
  let rem = ver
  for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25)
  return (ver << 12) | rem
}

function maskBit(mask: number, x: number, y: number): boolean {
  switch (mask) {
    case 0:
      return (x + y) % 2 === 0
    case 1:
      return y % 2 === 0
    case 2:
      return x % 3 === 0
    case 3:
      return (x + y) % 3 === 0
    case 4:
      return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0
    case 5:
      return ((x * y) % 2) + ((x * y) % 3) === 0
    case 6:
      return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0
    default:
      return ((((x + y) % 2) + ((x * y) % 3)) % 2) === 0
  }
}

/**
 * Encodes text (UTF-8) as a QR code at level M. Returns null when the text
 * does not fit into version 10.
 */
export function encodeQr(text: string): QrCode | null {
  const bytes = new TextEncoder().encode(text)
  let ver = 1
  while (ver <= MAX_VERSION && 4 + countBits(ver) + bytes.length * 8 > dataCodewords(ver) * 8) ver++
  if (ver > MAX_VERSION) return null

  const m = new Matrix(ver)
  m.drawFunctionPatterns()
  m.drawCodewords(addEccAndInterleave(encodeData(bytes, ver), ver))

  let best = 0
  let bestPenalty = Infinity
  for (let mask = 0; mask < 8; mask++) {
    m.applyMask(mask)
    m.drawFormatBits(mask)
    const p = m.penalty()
    if (p < bestPenalty) {
      best = mask
      bestPenalty = p
    }
    m.applyMask(mask)
  }
  m.applyMask(best)
  m.drawFormatBits(best)
  return { version: ver, size: m.size, modules: m.modules }
}

/**
 * SVG path data for the dark modules, offset by a quiet zone of `margin`
 * modules: one "M x y h1v1h-1z" square per module, merged per horizontal run.
 */
export function qrPath(qr: QrCode, margin = 4): string {
  const parts: string[] = []
  for (let y = 0; y < qr.size; y++) {
    let x = 0
    while (x < qr.size) {
      if (!qr.modules[y][x]) {
        x++
        continue
      }
      const start = x
      while (x < qr.size && qr.modules[y][x]) x++
      parts.push(`M${start + margin} ${y + margin}h${x - start}v1h-${x - start}z`)
    }
  }
  return parts.join('')
}
