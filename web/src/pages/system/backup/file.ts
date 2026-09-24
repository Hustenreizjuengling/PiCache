// Quick checks of a chosen backup file before it is uploaded. The server
// verifies integrity and schema version; this only catches the obvious
// mistakes (wrong file, empty, too large) without a 512 MiB upload.

/** The server's limit for POST /system/restore. */
export const MAX_BACKUP_BYTES = 512 * 1024 * 1024

/** Every SQLite 3 database starts with these 16 bytes. */
const SQLITE_MAGIC = 'SQLite format 3\u0000'

export type BackupFileProblem = 'empty' | 'tooLarge' | 'notSqlite' | 'unreadable'

/** Returns what is wrong with the file, or undefined if it looks like a backup. */
export async function checkBackupFile(f: Blob): Promise<BackupFileProblem | undefined> {
  if (f.size === 0) return 'empty'
  if (f.size > MAX_BACKUP_BYTES) return 'tooLarge'
  let head: Uint8Array
  try {
    head = new Uint8Array(await f.slice(0, SQLITE_MAGIC.length).arrayBuffer())
  } catch {
    return 'unreadable'
  }
  if (head.length < SQLITE_MAGIC.length) return 'notSqlite'
  for (let i = 0; i < SQLITE_MAGIC.length; i++) {
    if (head[i] !== SQLITE_MAGIC.charCodeAt(i)) return 'notSqlite'
  }
  return undefined
}
