// Saving files the page holds in memory (a fetched support bundle, the
// loaded application log): a temporary object URL on a download link.

/** "20260926T141503Z" (UTC) for file names. */
export function fileStamp(d: Date = new Date()): string {
  return d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z')
}

/** Offers a blob as a download under `name`. */
export function saveBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.append(a)
  a.click()
  a.remove()
  // The browser has taken the data once the download starts.
  setTimeout(() => URL.revokeObjectURL(url), 30_000)
}
