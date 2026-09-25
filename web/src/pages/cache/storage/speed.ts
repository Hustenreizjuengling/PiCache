// Storage speed test (POST/GET/DELETE /storage/benchmark): sizes, phases,
// units and the comparison with network speeds. The API reports bytes and
// bytes/s; the UI shows decimal MB/s (1 MB = 10^6 bytes) so that values
// compare directly with network speeds (1 GbE ≈ 118 MB/s of payload).

import { t } from '$i18n/index.svelte'
import type { BenchmarkPhase, BenchmarkResult, BenchmarkSize, BenchmarkThroughput } from '$lib/api'
import { formatBytes, formatNumber, formatRelative } from '$lib/format'

const MiB = 2 ** 20
const GiB = 2 ** 30
const NBSP = ' '

/** Sizes offered in the dialog (MiB) and the default. */
export const SIZES: readonly BenchmarkSize[] = [64, 256, 1024]
export const DEFAULT_SIZE: BenchmarkSize = 256

/** The test needs its size plus this much free space. */
const HEADROOM_BYTES = GiB

/** Free space the server requires for a test of `mib`. */
export function neededBytes(mib: number): number {
  return mib * MiB + HEADROOM_BYTES
}

/** Phases in the order they run. */
export const PHASES: readonly BenchmarkPhase[] = ['prepare', 'write', 'read', 'slices', 'metadata', 'cleanup', 'done']

/** Ethernet speeds to compare with: link rate in Gbit/s and usable payload in bytes/s. */
export const NETWORKS: readonly { gbit: number; bytesPerSec: number }[] = [
  { gbit: 1, bytesPerSec: 118e6 },
  { gbit: 2.5, bytesPerSec: 295e6 },
  { gbit: 10, bytesPerSec: 1180e6 },
]

/** "2.5 GbE" / "2,5 GbE". */
export function networkName(gbit: number): string {
  return `${formatNumber(gbit, 1)}${NBSP}GbE`
}

/** Within this share of each other, storage and network count as equally fast. */
const SAME_SPEED = 0.1

function valid(n: number | null | undefined): n is number {
  return typeof n === 'number' && Number.isFinite(n)
}

/** Bytes/s as decimal MB/s with about three significant digits: "1,180 MB/s", "98.4 MB/s", "5.12 MB/s". */
export function formatMBps(bytesPerSec: number | null | undefined): string {
  if (!valid(bytesPerSec)) return '–'
  const v = bytesPerSec / 1e6
  const digits = v >= 100 ? 0 : v >= 10 ? 1 : 2
  return `${formatNumber(v, digits)}${NBSP}MB/s`
}

/** Milliseconds: "0.42 ms", "3.4 ms", "24.8 ms", "130 ms". */
export function formatMs(ms: number | null | undefined): string {
  if (!valid(ms)) return '–'
  const digits = ms < 1 ? 2 : ms < 100 ? 1 : 0
  return `${formatNumber(ms, digits)}${NBSP}ms`
}

/** A test size in binary units as chosen in the dialog ("256 MiB", "1 GiB"); other sizes in decimal units. */
export function formatTestSize(bytes: number | null | undefined): string {
  if (!valid(bytes)) return '–'
  if (bytes > 0 && bytes % GiB === 0) return `${formatNumber(bytes / GiB)}${NBSP}GiB`
  if (bytes > 0 && bytes % MiB === 0) return `${formatNumber(bytes / MiB)}${NBSP}MiB`
  return formatBytes(bytes)
}

/** A size in MiB as offered in the dialog. */
export function formatMiB(mib: number): string {
  return formatTestSize(mib * MiB)
}

/** The phase was measured (partial results of a cancelled or failed run may lack it). */
export function measured(tp: BenchmarkThroughput | null | undefined): tp is BenchmarkThroughput {
  return !!tp && tp.bytes > 0 && tp.seconds > 0 && valid(tp.bytesPerSec) && tp.bytesPerSec > 0
}

/**
 * How fast downloads from the cache can come from this storage: the reads of
 * real cached content when measured (the hit path), else the sequential read.
 */
export function hitSpeed(r: BenchmarkResult): number | undefined {
  if (measured(r.slices)) return r.slices.bytesPerSec
  if (measured(r.read)) return r.read.bytesPerSec
  return undefined
}

export type Limit = 'storage' | 'network' | 'equal'

/** Which side limits downloads from the cache on a network of `network` bytes/s. */
export function limitFor(storage: number, network: number): Limit {
  if (Math.abs(storage - network) <= network * SAME_SPEED) return 'equal'
  return storage < network ? 'storage' : 'network'
}

/** "Last speed test: read 112 MB/s, write 98.4 MB/s, 5 minutes ago" (target card). */
export function lastLine(r: BenchmarkResult): string {
  return t('cache.speed.lastLine', {
    read: measured(r.read) ? formatMBps(r.read.bytesPerSec) : '–',
    write: measured(r.write) ? formatMBps(r.write.bytesPerSec) : '–',
    time: formatRelative(r.testedAt),
  })
}

/** A server message as a sentence (like errorText): first letter upper case. */
export function sentence(s: string): string {
  const v = s.trim()
  return v ? v[0].toUpperCase() + v.slice(1) : v
}

/** The server's notes (a Go nil slice arrives as null). */
export function notesOf(r: BenchmarkResult): string[] {
  return Array.isArray(r.notes) ? r.notes.filter((n) => typeof n === 'string' && n.trim() !== '').map(sentence) : []
}
