export const WATCH_POLL_MS = 5_000

export type FileStamp = { mtimeMs: number; size: number }

export const shouldReload = (prev: FileStamp | null, stat: FileStamp | null): boolean =>
  prev !== null && stat !== null && (prev.mtimeMs !== stat.mtimeMs || prev.size !== stat.size)
