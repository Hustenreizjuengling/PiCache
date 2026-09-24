// Cursor paging state for cursor lists (query log, cache requests): keeps the
// cursors of the pages visited so far so the Pager can go back.
//
//   const pages = new CursorStack()
//   const log = resource((s) => api.logs.queries({ ...filters, cursor: pages.current }, { signal: s }))
//   <Pager mode="cursor" hasPrev={pages.hasPrev} hasNext={!!log.data?.next}
//          onprev={() => pages.prev()} onnext={() => pages.next(log.data!.next!)} onfirst={() => pages.reset()} />
//   Call pages.reset() whenever a filter changes.

export class CursorStack {
  #stack = $state<string[]>([])

  /** Cursor of the current page ('' = first page). */
  get current(): string {
    return this.#stack.at(-1) ?? ''
  }

  /** Zero-based index of the current page. */
  get index(): number {
    return this.#stack.length
  }

  get hasPrev(): boolean {
    return this.#stack.length > 0
  }

  /** Moves to the page that starts at `cursor` (the `next` of the current page). */
  next(cursor: string): void {
    if (cursor) this.#stack.push(cursor)
  }

  prev(): void {
    this.#stack.pop()
  }

  /** Back to the first page. */
  reset(): void {
    if (this.#stack.length > 0) this.#stack = []
  }
}
