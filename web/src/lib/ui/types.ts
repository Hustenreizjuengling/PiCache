// Shared types of the UI components.

import type { Snippet } from 'svelte'
import type { IconName } from '../icons'

/** T568B pair colours; each has one fixed meaning (docs/DESIGN.md). */
export type Pair = 'blue' | 'orange' | 'green' | 'brown'

/** Status tones for health and messages (reuse green / warning / danger). */
export type Tone = 'neutral' | 'info' | 'ok' | 'warn' | 'fail'

/** A table column. `key` is also the sort key reported to onsort. */
export interface Column<T> {
  key: string
  label: string
  align?: 'left' | 'right' | 'center'
  /** Machine values (domains, IPs, paths, hashes): monospace. */
  mono?: boolean
  sortable?: boolean
  /** CSS width, e.g. '120px' or '30%'. */
  width?: string
  /** Value for client-side sorting and the default cell text. */
  value?: (row: T) => unknown
  /** Default cell text (defaults to String(value(row))). */
  format?: (row: T) => string
  /** Custom cell content (a snippet declared in the page's markup). */
  cell?: Snippet<[T]>
  /** Header tooltip / explanation. */
  title?: string
  /** Truncate long text with an ellipsis (full text in the title). */
  truncate?: boolean
}

/** Sort state of a Table. */
export interface SortState {
  key: string
  desc: boolean
}

/** An entry of a Menu. */
export type MenuItem =
  | {
      label: string
      icon?: IconName
      danger?: boolean
      disabled?: boolean
      /** Shows a check mark (radio-like menus). */
      checked?: boolean
      href?: string
      onselect?: () => void
    }
  | { separator: true }

/** A tab of Tabs. */
export interface TabItem {
  id: string
  label: string
  count?: number
  icon?: IconName
}

/** A row of KeyValue. */
export interface KeyValueItem {
  label: string
  value: string | number | null | undefined
  mono?: boolean
  href?: string
}

/** A segment of Meter. */
export interface MeterSegment {
  label: string
  value: number
  /** Formatted value for the legend (e.g. formatBytes(value)). */
  text?: string
  pair?: Pair
  tone?: Tone
}

/** An option of Select. */
export interface SelectOption {
  value: string
  label: string
  disabled?: boolean
}

/** A series of Chart (values aligned with the chart's timestamps). */
export interface ChartSeries {
  label: string
  values: (number | null)[]
  pair?: Pair
  /** A CSS colour variable name (e.g. '--text-3') for non-traffic series. */
  colorVar?: string
  /** Secondary state of the same meaning (dashed stroke). */
  dashed?: boolean
  /** Area fill (default true). */
  fill?: boolean
}
