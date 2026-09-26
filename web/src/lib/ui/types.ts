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
  /** Machine values (domains, IPs, paths, hashes): monospace, kept on one line. */
  mono?: boolean
  /** Lets a long mono value (a path, a URL) break anywhere instead of widening the table. */
  wrap?: boolean
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
  /**
   * Truncate long text with an ellipsis (full text in the title). The text
   * does not widen the column, so give it a `width` (e.g. '30%') and the
   * short one-line columns next to it '1%' (as wide as their content):
   * otherwise those take the free space and this column keeps only a minimum.
   */
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
      /** With href: a download link (the file name, or true for the server's). */
      download?: boolean | string
      onselect?: () => void
    }
  | { separator: true }
  /** A short explanation between the items (not focusable; it also describes the menu). */
  | { note: string }

/** An action of a BulkBar. */
export interface BulkAction {
  label: string
  icon?: IconName
  danger?: boolean
  disabled?: boolean
  /** Why the action is disabled (tooltip). */
  title?: string
  onselect: () => void
}

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
